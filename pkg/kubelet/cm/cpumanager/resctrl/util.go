package resctrl

import (
	"fmt"
	"reflect"
	"strconv"
)

// SetField sets obj's 'name' field with proper type
func SetField(obj interface{}, name string, value interface{}) error {
	structValue := reflect.ValueOf(obj).Elem()
	structFieldValue := structValue.FieldByName(name)

	if !structFieldValue.IsValid() {
		return fmt.Errorf("No such field: %s in obj", name)
	}

	if !structFieldValue.CanSet() {
		return fmt.Errorf("Cannot set %s field value", name)
	}

	val := reflect.ValueOf(value)
	structFieldType := structFieldValue.Type()

	switch structFieldType.Name() {
	// Try to remove these logic to TypeConversion
	case "int":
		v := value.(string)
		vInt, err := strconv.Atoi(v)
		if err != nil {
			// add log
			return err
		}
		val = reflect.ValueOf(vInt)
	}

	structFieldValue.Set(val)
	return nil
}

// SubtractStringSlice remove string from slice
func SubtractStringSlice(slice, s []string) []string {
	for _, i := range s {
		for pos, j := range slice {
			if i == j {
				slice = append(slice[:pos], slice[pos+1:]...)
				break
			}
		}
	}
	return slice
}
