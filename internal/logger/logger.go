// Package logger 提供全局结构化日志，封装 uber-go/zap。
// 通过 Init 初始化后，可直接调用包级函数（与 log 标准库签名兼容）。
package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var sugar *zap.SugaredLogger

func init() {
	// 进程启动时使用默认 logger，调用 Init 后会被替换
	build(true)
}

func build(development bool) {
	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		CallerKey:      "caller",
		MessageKey:     "msg",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05.000"),
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	if development {
		encoderCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		encoderCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	}

	level := zapcore.DebugLevel
	if !development {
		level = zapcore.InfoLevel
	}

	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		level,
	)
	// AddCallerSkip(1) 跳过本包的包级封装函数，指向真实调用位置
	l := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
	sugar = l.Sugar()
	zap.ReplaceGlobals(l)
}

// Init 在 main 启动时调用一次。development=true 时输出彩色可读格式，false 时输出纯文本（适合生产）。
func Init(development bool) { build(development) }

// Sync 刷新缓冲区，程序退出前调用。
func Sync() { _ = sugar.Sync() }

// ── 与 log 标准库同名的兼容封装 ─────────────────────────────────────────────

func Printf(format string, args ...interface{})  { sugar.Infof(format, args...) }
func Println(args ...interface{})               { sugar.Infoln(args...) }
func Print(args ...interface{})                 { sugar.Info(args...) }
func Fatalf(format string, args ...interface{}) { sugar.Fatalf(format, args...) }
func Fatalln(args ...interface{})               { sugar.Fatalln(args...) }
func Fatal(args ...interface{})                 { sugar.Fatal(args...) }

// ── 带层级的函数（可逐步替换 Printf 为更精确的级别）────────────────────────

func Debugf(format string, args ...interface{}) { sugar.Debugf(format, args...) }
func Infof(format string, args ...interface{})  { sugar.Infof(format, args...) }
func Warnf(format string, args ...interface{})  { sugar.Warnf(format, args...) }
func Errorf(format string, args ...interface{}) { sugar.Errorf(format, args...) }
func Debug(args ...interface{})                 { sugar.Debug(args...) }
func Info(args ...interface{})                  { sugar.Info(args...) }
func Warn(args ...interface{})                  { sugar.Warn(args...) }
func Error(args ...interface{})                 { sugar.Error(args...) }
