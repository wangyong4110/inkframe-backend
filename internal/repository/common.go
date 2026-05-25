package repository

import "strings"

// isSchemaMissing 判断是否为"列/表不存在"类错误（MySQL 1054/1146），遇到此类错误跳过而非中断
func isSchemaMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// MySQL: 1054 Unknown column / 1146 Table doesn't exist
	return strings.Contains(s, "1054") || strings.Contains(s, "1146") ||
		strings.Contains(s, "Unknown column") || strings.Contains(s, "doesn't exist") ||
		strings.Contains(s, "no such table") // SQLite
}

// isForeignKeyError 判断是否为 MySQL 外键约束错误（1452）
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1452") || strings.Contains(msg, "foreign key constraint")
}
