// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
//
// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT was not distributed with this file,
// You can obtain one at https://github.com/gogf/gf.

package gconv

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogf/gf/v2/internal/utils"
	"github.com/gogf/gf/v2/os/gtime"
	"github.com/gogf/gf/v2/util/gtag"
)

var (
	poolUsedParamsKeyOrTagNameMap = &sync.Pool{
		New: func() any {
			return make(map[string]struct{})
		},
	}
)

func poolGetUsedParamsKeyOrTagNameMap() map[string]struct{} {
	return poolUsedParamsKeyOrTagNameMap.Get().(map[string]struct{})
}

func poolPutUsedParamsKeyOrTagNameMap(m map[string]struct{}) {
	// need to be cleared, otherwise there will be a bug
	for k := range m {
		delete(m, k)
	}
	poolUsedParamsKeyOrTagNameMap.Put(m)
}

type cachedFieldInfoBase struct {
	// 字段的索引，可能是匿名嵌套的结构体，所以是[]int
	fieldIndexes []int

	// 字段的tag(可能是conv,param,p,c,json之类的),
	// priorityTagAndFieldName 包含字段的名字，该字段在数组最后一项。
	priorityTagAndFieldName []string

	// 1.iUnmarshalValue
	// 2.iUnmarshalText
	// 3.iUnmarshalJSON
	// 实现了以上3种接口的类型。
	// 目的：减少对每一个转换对象时，通过接口类型的运行时判断开销。
	isCommonInterface bool
	// 注册自定义转换的时候，比如func(src *int)(dest *string,err error)
	// 当结构体字段类型为string的时候，isCustomConvert 字段会为true
	// 表示此次转换有可能会是自定义转换，具体还需要进一步确定
	isCustomConvert bool
	structField     reflect.StructField

	// type Name struct{
	//     LastName  string
	//     FirstName string
	// }
	// type User struct{
	//     Name
	//     LastName  string
	//     FirstName string
	// }
	// 当结构体可能是类似于User结构体这种情况时
	// 只会存储两个字段LastName, FirstName使用不同的索引来代表不同的字段
	// 对于 LastName 字段来说
	// fieldIndex      = []int{0,1}
	// otherSameNameFieldIndex = [][]int{[]int{1}}长度只有1，因为只有一个重复的,且索引为1
	// 在赋值时会对这两个索引{0,1}和{1}都赋同样的值
	// 目前对于重复的字段可以做以下3种可能
	// 1.只设置第一个，后面重名的不设置
	// 2.只设置最后一个
	// 3.全部设置 (目前的做法)
	otherSameNameFieldIndex [][]int

	// 直接缓存字段的转换函数,对于简单的类型来说,相当于直接调用gconv.Int
	convertFunc func(from any, to reflect.Value)
}

type cachedFieldInfo struct {
	*cachedFieldInfoBase
	// 表示上次模糊匹配到的字段名字，可以缓存下来。
	// 如果用户没有设置tag之类的条件,
	// 而且字段名都匹配不上map的key时，缓存这个非常有用，可以省掉模糊匹配的开销。
	// TODO 如果不同的paramsMap含有不同格式的paramKey并且都命中同一个fieldName时，该缓存数值可能会不断更新。
	// lastFuzzKey string
	lastFuzzKey atomic.Value

	// 这个字段主要用在 bindStructWithLoopParamsMap 方法中，
	// 当map中同时存在一个字段的`fieldName`和`tag`时需要用到这个字段。
	// 例如为以下情况时:
	// field string `json:"name"`
	// map = {
	//	  "field" : "f1",
	//	  "name"  : "n1",
	// }
	// 这里应该以`name`为准,
	// 在 bindStructWithLoopParamsMap 方法中，由于`map`的无序性，可能会导致先遍历到`field`，
	// 这个字段更多的是表示优先级，即`name`的优先级比`field`的优先级高，即便之前已经设置过了。
	isField bool

	// removeSymbolsFieldName is used for quick fuzzy match for parameter key.
	// removeSymbolsFieldName = utils.RemoveSymbols(fieldName)
	removeSymbolsFieldName string
}

func (cfi *cachedFieldInfo) FieldName() string {
	return cfi.priorityTagAndFieldName[len(cfi.priorityTagAndFieldName)-1]
}

func (cfi *cachedFieldInfo) getFieldReflectValue(structValue reflect.Value) reflect.Value {
	if len(cfi.fieldIndexes) == 1 {
		return structValue.Field(cfi.fieldIndexes[0])
	}
	return cfi.fieldReflectValue(structValue, cfi.fieldIndexes)
}

func (cfi *cachedFieldInfo) getOtherFieldReflectValue(structValue reflect.Value, fieldLevel int) reflect.Value {
	fieldIndex := cfi.otherSameNameFieldIndex[fieldLevel]
	if len(fieldIndex) == 1 {
		return structValue.Field(fieldIndex[0])
	}
	return cfi.fieldReflectValue(structValue, fieldIndex)
}

type cachedStructInfo struct {
	// This map field is mainly used in the [bindStructWithLoopParamsMap] method
	// key = field's name
	// Will save all field names and priorityTagAndFieldName
	// for example：
	//	field string `json:"name"`
	// It will be stored twice
	// 属性名称/标签名称到缓存Field对象信息的映射。
	tagOrFiledNameToFieldInfoMap map[string]*cachedFieldInfo

	// All sub attributes field info slice.
	fieldConvertInfos []*cachedFieldInfo
}

func (csi *cachedStructInfo) HasNoFields() bool {
	return len(csi.tagOrFiledNameToFieldInfoMap) == 0
}

func (csi *cachedStructInfo) GetFieldInfo(fieldName string) *cachedFieldInfo {
	return csi.tagOrFiledNameToFieldInfoMap[fieldName]
}

func (csi *cachedStructInfo) AddField(field reflect.StructField, fieldIndexes []int, priorityTags []string) {
	alreadyExistFieldInfo, ok := csi.tagOrFiledNameToFieldInfoMap[field.Name]
	if !ok {
		baseInfo := &cachedFieldInfoBase{
			isCommonInterface:       checkTypeIsImplCommonInterface(field),
			structField:             field,
			fieldIndexes:            fieldIndexes,
			convertFunc:             genFieldConvertFunc(field.Type.String()),
			isCustomConvert:         checkTypeMaybeIsCustomConvert(field.Type), // TODO merged to convertFunc?
			priorityTagAndFieldName: genPriorityTagAndFieldName(field, priorityTags),
		}
		for _, tagOrFieldName := range baseInfo.priorityTagAndFieldName {
			newFieldInfo := &cachedFieldInfo{
				cachedFieldInfoBase:    baseInfo,
				isField:                tagOrFieldName == field.Name,
				removeSymbolsFieldName: utils.RemoveSymbols(field.Name),
			}
			newFieldInfo.lastFuzzKey.Store(field.Name)
			csi.tagOrFiledNameToFieldInfoMap[tagOrFieldName] = newFieldInfo
			if newFieldInfo.isField {
				// TODO 为什么只有isField才添加到fieldConvertInfos
				csi.fieldConvertInfos = append(csi.fieldConvertInfos, newFieldInfo)
			}
		}
		return
	}
	if alreadyExistFieldInfo.otherSameNameFieldIndex == nil {
		alreadyExistFieldInfo.otherSameNameFieldIndex = make([][]int, 0, 2)
	}
	alreadyExistFieldInfo.otherSameNameFieldIndex = append(
		alreadyExistFieldInfo.otherSameNameFieldIndex,
		fieldIndexes,
	)
	return
}

var (
	// Used to store whether field types are registered to custom conversions
	// For example:
	// func (src *TypeA) (dst *TypeB,err error)
	// This map will store TypeB for quick judgment during assignment
	customConvTypeMap = map[reflect.Type]struct{}{}
)

func registerCacheConvFieldCustomType(fieldType reflect.Type) {
	if fieldType.Kind() == reflect.Ptr {
		fieldType = fieldType.Elem()
	}
	customConvTypeMap[fieldType] = struct{}{}
}

func checkTypeMaybeIsCustomConvert(fieldType reflect.Type) bool {
	if fieldType.Kind() == reflect.Ptr {
		fieldType = fieldType.Elem()
	}
	_, ok := customConvTypeMap[fieldType]
	return ok
}

func genPtrConvertFunc(convertFunc func(from any, to reflect.Value)) func(from any, to reflect.Value) {
	return func(from any, to reflect.Value) {
		if to.IsNil() {
			to.Set(reflect.New(to.Type().Elem()))
		}
		convertFunc(from, to.Elem())
	}
}

func genFieldConvertFunc(fieldType string) (convertFunc func(from any, to reflect.Value)) {
	if fieldType[0] == '*' {
		convertFunc = genFieldConvertFunc(fieldType[1:])
		if convertFunc == nil {
			return nil
		}
		return genPtrConvertFunc(convertFunc)
	}
	switch fieldType {
	case "int", "int8", "int16", "int32", "int64":
		convertFunc = func(from any, to reflect.Value) {
			to.SetInt(Int64(from))
		}
	case "uint", "uint8", "uint16", "uint32", "uint64":
		convertFunc = func(from any, to reflect.Value) {
			to.SetUint(Uint64(from))
		}
	case "string":
		convertFunc = func(from any, to reflect.Value) {
			to.SetString(String(from))
		}
	case "float32":
		convertFunc = func(from any, to reflect.Value) {
			to.SetFloat(float64(Float32(from)))
		}
	case "float64":
		convertFunc = func(from any, to reflect.Value) {
			to.SetFloat(Float64(from))
		}
	case "Time", "time.Time":
		convertFunc = func(from any, to reflect.Value) {
			*to.Addr().Interface().(*time.Time) = Time(from)
		}
	case "GTime", "gtime.Time":
		convertFunc = func(from any, to reflect.Value) {
			v := GTime(from)
			if v == nil {
				v = gtime.New()
			}
			*to.Addr().Interface().(*gtime.Time) = *v
		}
	case "bool":
		convertFunc = func(from any, to reflect.Value) {
			to.SetBool(Bool(from))
		}
	case "[]byte":
		convertFunc = func(from any, to reflect.Value) {
			to.SetBytes(Bytes(from))
		}
	default:
		return nil
	}
	return convertFunc
}

var (
	// map[reflect.Type]*cachedStructInfo
	cachedStructsInfoMap = sync.Map{}
)

func setCachedConvertStructInfo(structType reflect.Type, info *cachedStructInfo) {
	// Temporarily enabled as an experimental feature
	cachedStructsInfoMap.Store(structType, info)
}

func getCachedConvertStructInfo(structType reflect.Type) (*cachedStructInfo, bool) {
	// Temporarily enabled as an experimental feature
	v, ok := cachedStructsInfoMap.Load(structType)
	if ok {
		return v.(*cachedStructInfo), ok
	}
	return nil, false
}

func getCachedStructInfo(structType reflect.Type, priorityTag string) *cachedStructInfo {
	if structType.Kind() != reflect.Struct {
		return nil
	}
	// Check if it has been cached
	structInfo, ok := getCachedConvertStructInfo(structType)
	if ok {
		return structInfo
	}
	structInfo = &cachedStructInfo{
		tagOrFiledNameToFieldInfoMap: make(map[string]*cachedFieldInfo),
	}
	var (
		priorityTagArray []string
		parentIndex      = make([]int, 0)
	)
	if priorityTag != "" {
		priorityTagArray = append(utils.SplitAndTrim(priorityTag, ","), gtag.StructTagPriority...)
	} else {
		priorityTagArray = gtag.StructTagPriority
	}
	parseStruct(structType, parentIndex, structInfo, priorityTagArray)
	setCachedConvertStructInfo(structType, structInfo)
	return structInfo
}

func parseStruct(
	structType reflect.Type,
	fieldIndexes []int,
	structInfo *cachedStructInfo,
	priorityTagArray []string,
) {
	var (
		fieldName   string
		structField reflect.StructField
		fieldType   reflect.Type
	)
	// TODO:
	//  Check if the structure has already been cached in the cache.
	//  If it has been cached, some information can be reused,
	//  but the [FieldIndex] needs to be reset.
	//  We will not implement it temporarily because it is somewhat complex
	for i := 0; i < structType.NumField(); i++ {
		structField = structType.Field(i)
		fieldType = structField.Type
		fieldName = structField.Name
		// Only do converting to public attributes.
		if !utils.IsLetterUpper(fieldName[0]) {
			continue
		}

		// store field
		structInfo.AddField(structField, append(fieldIndexes, i), priorityTagArray)

		// normal basic attributes.
		if structField.Anonymous {
			// handle struct attributes, it might be struct/*struct embedded..
			if fieldType.Kind() == reflect.Ptr {
				fieldType = fieldType.Elem()
			}
			if fieldType.Kind() != reflect.Struct {
				continue
			}
			if structField.Tag != "" {
				// TODO: If it's an anonymous field with a tag, doesn't it need to be recursive?
			}
			parseStruct(fieldType, append(fieldIndexes, i), structInfo, priorityTagArray)
		}
	}
}

func (cfi *cachedFieldInfo) fieldReflectValue(v reflect.Value, fieldIndexes []int) reflect.Value {
	for i, x := range fieldIndexes {
		if i > 0 {
			switch v.Kind() {
			case reflect.Pointer:
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()

			case reflect.Interface:
				// Compatible with previous code
				// Interface => struct
				v = v.Elem()
				if v.Kind() == reflect.Ptr {
					// maybe *struct or other types
					v = v.Elem()
				}
			}
		}
		v = v.Field(x)
	}
	return v
}
func genPriorityTagAndFieldName(field reflect.StructField, priorityTags []string) (priorityTagAndFieldName []string) {
	for _, tag := range priorityTags {
		value, ok := field.Tag.Lookup(tag)
		if ok {
			// If there's something else in the tag string,
			// it uses the first part which is split using char ','.
			// Example:
			// orm:"id, priority"
			// orm:"name, with:uid=id"
			tagValueItems := strings.Split(value, ",")
			// json:",omitempty"
			trimmedTagName := strings.TrimSpace(tagValueItems[0])
			if trimmedTagName != "" {
				priorityTagAndFieldName = append(priorityTagAndFieldName, trimmedTagName)
				break
			}
		}
	}
	priorityTagAndFieldName = append(priorityTagAndFieldName, field.Name)
	return
}

var (
	implUnmarshalText  = reflect.TypeOf((*iUnmarshalText)(nil)).Elem()
	implUnmarshalJson  = reflect.TypeOf((*iUnmarshalJSON)(nil)).Elem()
	implUnmarshalValue = reflect.TypeOf((*iUnmarshalValue)(nil)).Elem()
)

func checkTypeIsImplCommonInterface(field reflect.StructField) bool {
	isCommonInterface := false
	switch field.Type.String() {
	case "time.Time", "*time.Time":
	case "gtime.Time", "*gtime.Time":
		// default convert
	default:
		// Implemented three types of interfaces that must be pointer types, otherwise it is meaningless
		if field.Type.Kind() != reflect.Ptr {
			field.Type = reflect.PointerTo(field.Type)
		}
		switch {
		case field.Type.Implements(implUnmarshalText):
			isCommonInterface = true
		case field.Type.Implements(implUnmarshalJson):
			isCommonInterface = true
		case field.Type.Implements(implUnmarshalValue):
			isCommonInterface = true
		}
	}
	return isCommonInterface
}
