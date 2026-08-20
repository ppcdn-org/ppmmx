package validator

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"scanorder/internal/errors"
)

// Validator 验证器
type Validator struct{}

// New 创建验证器实例
func New() *Validator {
	return &Validator{}
}

// ValidateStruct 验证结构体
func (v *Validator) ValidateStruct(s interface{}) error {
	val := reflect.ValueOf(s)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct {
		return nil
	}

	return v.validateStruct(val)
}

// validateStruct 递归验证结构体
func (v *Validator) validateStruct(val reflect.Value) error {
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// 解析验证标签
		tag := fieldType.Tag.Get("validate")
		if tag == "" {
			continue
		}

		fieldName := fieldType.Name
		if jsonTag := fieldType.Tag.Get("json"); jsonTag != "" {
			fieldName = jsonTag
		}

		if err := v.validateField(field, tag, fieldName); err != nil {
			return err
		}

		// 如果是嵌套结构体，递归验证
		if field.Kind() == reflect.Struct {
			if err := v.validateStruct(field); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateField 验证字段
func (v *Validator) validateField(field reflect.Value, tag, fieldName string) error {
	rules := strings.Split(tag, ",")

	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}

		if err := v.validateRule(field, rule, fieldName); err != nil {
			return err
		}
	}

	return nil
}

// validateRule 验证规则
func (v *Validator) validateRule(field reflect.Value, rule, fieldName string) error {
	switch {
	case rule == "required":
		if v.isEmpty(field) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s is required", fieldName), 400)
		}
	case strings.HasPrefix(rule, "min="):
		minStr := strings.TrimPrefix(rule, "min=")
		if err := v.validateMin(field, minStr, fieldName); err != nil {
			return err
		}
	case strings.HasPrefix(rule, "max="):
		maxStr := strings.TrimPrefix(rule, "max=")
		if err := v.validateMax(field, maxStr, fieldName); err != nil {
			return err
		}
	case strings.HasPrefix(rule, "len="):
		lenStr := strings.TrimPrefix(rule, "len=")
		if err := v.validateLen(field, lenStr, fieldName); err != nil {
			return err
		}
	case rule == "email":
		if str, ok := field.Interface().(string); ok {
			if !v.isValidEmail(str) {
				return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be a valid email", fieldName), 400)
			}
		}
	case rule == "phone":
		if str, ok := field.Interface().(string); ok {
			if !v.isValidPhone(str) {
				return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be a valid phone number", fieldName), 400)
			}
		}
	}

	return nil
}

// isEmpty 检查是否为空
func (v *Validator) isEmpty(field reflect.Value) bool {
	switch field.Kind() {
	case reflect.String:
		return field.String() == ""
	case reflect.Ptr, reflect.Interface:
		return field.IsNil()
	case reflect.Slice, reflect.Map, reflect.Array:
		return field.Len() == 0
	}
	return false
}

// validateMin 验证最小值
func (v *Validator) validateMin(field reflect.Value, minStr, fieldName string) error {
	switch field.Kind() {
	case reflect.String:
		if len(field.String()) < parseInt(minStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at least %s characters", fieldName, minStr), 400)
		}
	case reflect.Int, reflect.Int32, reflect.Int64:
		if field.Int() < parseInt64(minStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at least %s", fieldName, minStr), 400)
		}
	case reflect.Float32, reflect.Float64:
		if field.Float() < parseFloat(minStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at least %s", fieldName, minStr), 400)
		}
	}
	return nil
}

// validateMax 验证最大值
func (v *Validator) validateMax(field reflect.Value, maxStr, fieldName string) error {
	switch field.Kind() {
	case reflect.String:
		if len(field.String()) > parseInt(maxStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at most %s characters", fieldName, maxStr), 400)
		}
	case reflect.Int, reflect.Int32, reflect.Int64:
		if field.Int() > parseInt64(maxStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at most %s", fieldName, maxStr), 400)
		}
	case reflect.Float32, reflect.Float64:
		if field.Float() > parseFloat(maxStr) {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be at most %s", fieldName, maxStr), 400)
		}
	}
	return nil
}

// validateLen 验证长度
func (v *Validator) validateLen(field reflect.Value, lenStr, fieldName string) error {
	expectedLen := parseInt(lenStr)
	switch field.Kind() {
	case reflect.String:
		if len(field.String()) != expectedLen {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must be exactly %s characters", fieldName, lenStr), 400)
		}
	case reflect.Slice, reflect.Array:
		if field.Len() != expectedLen {
			return errors.New("VALIDATION_ERROR", fmt.Sprintf("%s must have exactly %s items", fieldName, lenStr), 400)
		}
	}
	return nil
}

// isValidEmail 验证邮箱
func (v *Validator) isValidEmail(email string) bool {
	pattern := `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`
	match, _ := regexp.MatchString(pattern, email)
	return match
}

// isValidPhone 验证手机号
func (v *Validator) isValidPhone(phone string) bool {
	// 菲律宾手机号格式
	pattern := `^(?:\+63|63|0)?[9]\d{9}$`
	match, _ := regexp.MatchString(pattern, phone)
	return match
}

// 辅助函数
func parseInt(s string) int {
	var result int
	fmt.Sscanf(s, "%d", &result)
	return result
}

func parseInt64(s string) int64 {
	var result int64
	fmt.Sscanf(s, "%d", &result)
	return result
}

func parseFloat(s string) float64 {
	var result float64
	fmt.Sscanf(s, "%f", &result)
	return result
}
