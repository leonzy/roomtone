package version

// VersionBannerPrefix 是 `-version` 输出里版本号行的前缀。
// 它是升级/热替换逻辑的解析契约（docker-entrypoint.sh 与 UpgradeService 都按它取值），
// 改名时必须与脚本、测试同步修改。
const VersionBannerPrefix = "Roomtone Version:"

// LegacyVersionBannerPrefix 是旧版 Songloft 二进制的同名前缀，仅用于解析兼容。
const LegacyVersionBannerPrefix = "Songloft Version:"

// 这些变量将在编译时通过 -ldflags 注入
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
	BuildType = "" // 构建类型，"lite" 或 ""（默认 full）
)

// FormatVersionLine 返回 `-version` 输出中版本号那一行。
func FormatVersionLine() string {
	return VersionBannerPrefix + " " + GetVersion()
}

// GetVersion 返回版本号
func GetVersion() string {
	return Version
}

// GetFullVersion 返回包含 Git 提交和构建时间的完整版本信息
func GetFullVersion() string {
	s := Version + " (commit: " + GitCommit + ", built: " + BuildTime
	if BuildType != "" {
		s += ", type: " + BuildType
	}
	s += ")"
	return s
}
