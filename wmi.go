package wmic

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var fieldCache = map[string]string{}

const TIMEOUT_DEFAULT = "30m"

// RecordError holds information about an error for record in the WMI result
type RecordError struct {
	Class   string
	Field   string
	Line    int
	Message string
}

// FieldError is an error for a missing field
type FieldError struct {
	Field string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("Cannot find field %s, names are case-sensitive", e.Field)
}

// UnsupportedTypeError is an error for a field type that isn't supported
type UnsupportedTypeError struct {
	Field string
	Type  string
}

func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf("Field %s has an unsupported type %s", e.Field, e.Type)
}

// QueryAll returns all items with columns matching the out struct
func QueryAll(class string, out interface{}) ([]RecordError, error) {
	return Query(class, []string{}, "", out)
}

func QueryAllWithTimeout(class string, out interface{}, timeout string) ([]RecordError, error) {
	return QueryWithTimeout(class, []string{}, "", out, timeout)
}

// QueryColumns returns all items with specific columns
func QueryColumns(class string, columns []string, out interface{}) ([]RecordError, error) {
	return Query(class, columns, "", out)
}

func QueryColumnsWithTimeout(class string, columns []string, out interface{}, timeout string) ([]RecordError, error) {
	return QueryWithTimeout(class, columns, "", out, timeout)
}

// QueryWhere returns all columns for where clause
func QueryWhere(class, where string, out interface{}) ([]RecordError, error) {
	return Query(class, []string{}, where, out)
}

func QueryWhereWithTimeout(class, where string, out interface{}, timeout string) ([]RecordError, error) {
	return QueryWithTimeout(class, []string{}, where, out, timeout)
}

// Query returns a WMI query with the given parameters
func Query(class string, columns []string, where string, out interface{}) ([]RecordError, error) {
	return QueryWithTimeout(class, []string{}, where, out, TIMEOUT_DEFAULT)
}

func QueryWithTimeout(class string, columns []string, where string, out interface{}, timeout string) ([]RecordError, error) {

	recordErrors := []RecordError{}

	// Get the outer type (needs to be a slice)
	outerValue := reflect.ValueOf(out)
	if outerValue.Kind() == reflect.Ptr {
		outerValue = outerValue.Elem()
	}

	if outerValue.Kind() != reflect.Slice {
		return recordErrors, fmt.Errorf("You must provide a slice to the out argument")
	}

	// Get the inner type of the slice
	innerType := outerValue.Type().Elem()
	innerTypeIsPointer := false
	if innerType.Kind() == reflect.Ptr {
		// If a pointer get the underlying type
		innerTypeIsPointer = true
		innerType = innerType.Elem()
	}

	if innerType.Kind() != reflect.Struct {
		return recordErrors, fmt.Errorf("You must provide a struct as the type of the out slice")
	}

	query := []string{"PATH", class}
	if where != "" {
		parts := strings.Split(strings.TrimSpace(where), " ")
		query = append(query, "WHERE")
		if !strings.HasPrefix(parts[0], "(") {
			query = append(query, "(")
		}
		query = append(query, parts...)
		if !strings.HasSuffix(parts[len(parts)-1], ")") {
			query = append(query, ")")
		}
	}
	query = append(query, "GET")

	// If the column list is empty use the struct to create the get list
	if len(columns) == 0 {
		structName := innerType.Name()
		if val, ok := fieldCache[structName]; ok {
			query = append(query, val)
		} else {
			cols := []string{}
			for i := 0; i < innerType.NumField(); i++ {
				n := innerType.Field(i).Name
				cols = append(cols, n)
			}
			colString := strings.Join(cols, ",")
			fieldCache[structName] = colString
			query = append(query, colString)
		}
	} else {
		query = append(query, strings.Join(columns, ","))
	}
	query = append(query, "/format:rawxml")
	query = append(query, "/VALUE")

	duration, errParse := time.ParseDuration(timeout)
	if errParse != nil {
		return recordErrors, errParse
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wmic", query...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return recordErrors, err
	}
	if stderr.Len() > 0 {
		return recordErrors, errors.New(string(stderr.Bytes()))
	}

	result := make([]interface{}, 0)

	// Loop over the string
	str := string(stdout.Bytes())
	scanner := bufio.NewScanner(strings.NewReader(str))
	item := reflect.New(innerType).Interface()
	contentStarted := false
	line := 1
	for scanner.Scan() {
		s := strings.TrimSpace(scanner.Text())
		if s == "" {
			if contentStarted {
				line++
				result = append(result, item)
				item = reflect.New(innerType).Interface()
				contentStarted = false
			}
		} else {
			contentStarted = true
			parts := strings.SplitN(s, "=", 2)
			if len(parts) == 2 {
				param := parts[0]
				val := strings.TrimSpace(parts[1])
				if val != "" {
					err = set(param, val, item)
					if err != nil {
						if _, ok := err.(*FieldError); ok {
							return recordErrors, err
						} else if _, ok := err.(*UnsupportedTypeError); ok {
							return recordErrors, err
						}
						// Error that allows continuation
						recordErrors = append(recordErrors, RecordError{Class: class, Field: param, Line: line, Message: err.Error()})
					}
				}
			}
		}
	}

	if contentStarted {
		// Add remaining item if there is one
		result = append(result, item)
	}

	// Resize the out slice to match the number of records
	outerValue.Set(reflect.MakeSlice(outerValue.Type(), len(result), len(result)))

	for i, val := range result {
		// Update the out slice with each item
		v := reflect.ValueOf(val)
		if innerTypeIsPointer {
			outerValue.Index(i).Set(v)
		} else {
			outerValue.Index(i).Set(v.Elem())
		}
	}

	return recordErrors, nil
}

func set(field, s string, item interface{}) error {
	v := reflect.ValueOf(item)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return &FieldError{Field: field}
	}
	switch f.Kind() {
	case reflect.String:
		return setString(s, f)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setIntN(s, f, f.Type().Bits())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUintN(s, f, f.Type().Bits())
	case reflect.Float32, reflect.Float64:
		return setFloatN(s, f, f.Type().Bits())
	case reflect.Bool:
		return setBool(s, f)
	}
	return &UnsupportedTypeError{Field: field, Type: f.Kind().String()}
}

func setString(s string, v reflect.Value) error {
	v.SetString(s)
	return nil
}

func setIntN(s string, v reflect.Value, bits int) error {
	n, err := strconv.ParseInt(s, 10, bits)
	if err != nil {
		return fmt.Errorf("Unable to set field %s type %s", v.Type().Name, s)
	}
	v.SetInt(n)
	return nil
}

func setUintN(s string, v reflect.Value, bits int) error {
	n, err := strconv.ParseUint(s, 10, bits)
	if err != nil {
		return fmt.Errorf("Unable to set field %s type %s", v.Type().Name, s)
	}
	v.SetUint(n)
	return nil
}

func setFloatN(s string, v reflect.Value, bits int) error {
	n, err := strconv.ParseFloat(s, bits)
	if err != nil {
		return fmt.Errorf("Unable to set field %s type %s", v.Type().Name, s)
	}
	v.SetFloat(n)
	return nil
}

func setBool(s string, v reflect.Value) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("Unable to set field %s type %s", v.Type().Name, s)
	}
	v.SetBool(b)
	return nil
}
