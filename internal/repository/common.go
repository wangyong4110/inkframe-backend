package repository

import "strings"

// isForeignKeyError 判断是否为 MySQL 外键约束错误（1452）
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1452") || strings.Contains(msg, "foreign key constraint")
}
