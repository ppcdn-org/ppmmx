package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"time"

	"scanorder/internal/config"
)

type Logger struct {
	infoLogger  *log.Logger
	errorLogger *log.Logger
	debugLogger *log.Logger
}

type Level int

const (
	Debug Level = iota
	Info
	Error
)

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Error:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// NewLogger 创建日志实例
func NewLogger(cfg *config.AppConfig) *Logger {
	var infoWriter, errorWriter, debugWriter io.Writer

	if cfg.Env == "development" {
		infoWriter = os.Stdout
		errorWriter = os.Stderr
		debugWriter = os.Stdout
	} else {
		// 生产环境可以写到文件
		infoWriter = os.Stdout
		errorWriter = os.Stderr
		debugWriter = os.Stdout
	}

	return &Logger{
		infoLogger:  log.New(infoWriter, "", 0),
		errorLogger: log.New(errorWriter, "", 0),
		debugLogger: log.New(debugWriter, "", 0),
	}
}

// formatMessage 格式化日志消息
func (l *Logger) formatMessage(level Level, message string) string {
	_, file, line, _ := runtime.Caller(2)
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("[%s] [%s] %s:%d %s", timestamp, level.String(), file, line, message)
}

// Info 记录信息日志
func (l *Logger) Info(message string) {
	l.infoLogger.Println(l.formatMessage(Info, message))
}

// Infof 记录格式化信息日志
func (l *Logger) Infof(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.Info(message)
}

// Error 记录错误日志
func (l *Logger) Error(message string) {
	l.errorLogger.Println(l.formatMessage(Error, message))
}

// Errorf 记录格式化错误日志
func (l *Logger) Errorf(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.Error(message)
}

// Debug 记录调试日志
func (l *Logger) Debug(message string) {
	if os.Getenv("APP_ENV") == "development" {
		l.debugLogger.Println(l.formatMessage(Debug, message))
	}
}

// Debugf 记录格式化调试日志
func (l *Logger) Debugf(format string, args ...interface{}) {
	if os.Getenv("APP_ENV") == "development" {
		message := fmt.Sprintf(format, args...)
		l.Debug(message)
	}
}
