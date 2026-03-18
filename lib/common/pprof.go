package common

// InitPProfByAddr 在精简版中保留空实现，避免拉入 pprof 调试依赖。
func InitPProfByAddr(addr string) {
	_ = addr
}
