package common

import (
	"encoding/binary"
	"time"
)

// SetNtpServer 在精简版中保留空实现，确保旧调用点仍可编译。
func SetNtpServer(server string) {
	_ = server
}

// SetNtpInterval 在精简版中保留空实现，NTP 同步逻辑已移除。
func SetNtpInterval(d time.Duration) {
	_ = d
}

// CalibrateTimeOffset 精简版不再做 NTP 校时，始终返回 0 偏移。
func CalibrateTimeOffset(server string) (time.Duration, error) {
	_ = server
	return 0, nil
}

func TimeOffset() time.Duration {
	return 0
}

// TimeNow 返回系统当前时间，去掉了额外校时与时区嵌入数据。
func TimeNow() time.Time {
	return time.Now()
}

// SyncTime 精简版中为空实现，避免拉入 NTP 依赖。
func SyncTime() {
}

// SetTimezone 精简版不再处理时区切换，保持空实现以兼容旧调用。
func SetTimezone(tz string) error {
	_ = tz
	return nil
}

// TimestampToBytes 8bit
func TimestampToBytes(ts int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(ts))
	return b
}

// BytesToTimestamp 8bit
func BytesToTimestamp(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b))
}
