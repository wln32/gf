// Copyright GoFrame Author(https://goframe.org). All Rights Reserved.
//
// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT was not distributed with this file,
// You can obtain one at https://github.com/gogf/gf.

package structcache

import (
	"reflect"
	"sync/atomic"
)

// CachedFieldInfo holds the cached info for struct field.
type CachedFieldInfo struct {
	// 字段的索引，可能是匿名嵌套的结构体，所以是[]int
	FieldIndexes []int

	// 字段的tag(可能是conv,param,p,c,json之类的),
	// PriorityTagAndFieldName 包含字段的名字，该字段在数组最后一项。
	PriorityTagAndFieldName []string

	// 1.iUnmarshalValue
	// 2.iUnmarshalText
	// 3.iUnmarshalJSON
	// 实现了以上3种接口的类型。
	// 目的：减少对每一个转换对象时，通过接口类型的运行时判断开销。
	IsCommonInterface bool

	// 注册自定义转换的时候，比如func(src *int)(dest *string,err error)
	// 当结构体字段类型为string的时候，IsCustomConvert 字段会为true
	// 表示此次转换有可能会是自定义转换，具体还需要进一步确定
	IsCustomConvert bool

	StructField reflect.StructField

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
	// OtherSameNameFieldIndex = [][]int{[]int{1}}长度只有1，因为只有一个重复的,且索引为1
	// 在赋值时会对这两个索引{0,1}和{1}都赋同样的值
	// 目前对于重复的字段可以做以下3种可能
	// 1.只设置第一个，后面重名的不设置
	// 2.只设置最后一个
	// 3.全部设置 (目前的做法)
	OtherSameNameFieldIndex [][]int

	// 直接缓存字段的转换函数,对于简单的类型来说,相当于直接调用gconv.Int
	ConvertFunc func(from any, to reflect.Value)

	// 表示上次模糊匹配到的字段名字，可以缓存下来。
	// 如果用户没有设置tag之类的条件,
	// 而且字段名都匹配不上map的key时，缓存这个非常有用，可以省掉模糊匹配的开销。
	// TODO 如果不同的paramsMap含有不同格式的paramKey并且都命中同一个fieldName时，该缓存数值可能会不断更新。
	// lastFuzzKey string
	LastFuzzKey atomic.Value

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
	IsField bool

	// removeSymbolsFieldName is used for quick fuzzy match for parameter key.
	// removeSymbolsFieldName = utils.RemoveSymbols(fieldName)
	RemoveSymbolsFieldName string
}

// FieldName returns the field name of current field info.
func (cfi *CachedFieldInfo) FieldName() string {
	return cfi.PriorityTagAndFieldName[len(cfi.PriorityTagAndFieldName)-1]
}

// GetFieldReflectValue retrieves and returns the reflect.Value of given struct value,
// which is used for directly value assignment.
func (cfi *CachedFieldInfo) GetFieldReflectValue(structValue reflect.Value) reflect.Value {
	if len(cfi.FieldIndexes) == 1 {
		return structValue.Field(cfi.FieldIndexes[0])
	}
	return cfi.fieldReflectValue(structValue, cfi.FieldIndexes)
}

// GetOtherFieldReflectValue retrieves and returns the reflect.Value of given struct value with nested index
// by `fieldLevel`, which is used for directly value assignment.
func (cfi *CachedFieldInfo) GetOtherFieldReflectValue(structValue reflect.Value, fieldLevel int) reflect.Value {
	fieldIndex := cfi.OtherSameNameFieldIndex[fieldLevel]
	if len(fieldIndex) == 1 {
		return structValue.Field(fieldIndex[0])
	}
	return cfi.fieldReflectValue(structValue, fieldIndex)
}

func (cfi *CachedFieldInfo) fieldReflectValue(v reflect.Value, fieldIndexes []int) reflect.Value {
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
