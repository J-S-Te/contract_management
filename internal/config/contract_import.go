package config

import (
	"fmt"
	"strconv"
	"time"
)

// loadImportConfig 加载默认关闭的PDF及批次入口并校验资源边界。
// 参数：c为待补齐的合同进程配置，不读取任何业务数据库。
// 返回值：配置合法时为nil；非法值只返回变量名和约束，避免泄漏配置内容。
func loadImportConfig(c *Config) error {
	flags := map[string]*bool{
		"CONTRACT_EXTERNAL_PDF_ENABLED":   &c.ExternalPDFEnabled,
		"CONTRACT_IMPORT_ENABLED":         &c.ContractImportEnabled,
		"CONTRACT_IMPORT_HISTORY_ENABLED": &c.ContractImportHistoryEnabled,
	}
	for key, target := range flags {
		value, err := strconv.ParseBool(env(key, "false"))
		if err != nil {
			return fmt.Errorf("%s must be a boolean", key)
		}
		*target = value
	}
	var err error
	if c.ContractImportMaxItems, err = boundedImportInt("CONTRACT_IMPORT_MAX_ITEMS", "100", 100); err != nil {
		return err
	}
	if c.ContractImportMaxConcurrency, err = boundedImportInt("CONTRACT_IMPORT_MAX_CONCURRENCY", "1", 4); err != nil {
		return err
	}
	ttl, parseErr := time.ParseDuration(env("CONTRACT_IMPORT_STAGING_TTL", "168h"))
	if parseErr != nil || ttl < 24*time.Hour || ttl > 720*time.Hour {
		return fmt.Errorf("CONTRACT_IMPORT_STAGING_TTL must be between 24h and 720h")
	}
	c.ContractImportStagingTTL = ttl
	return nil
}

// boundedImportInt 读取有界并发或批次数，禁止无限或溢出配置。
// 参数：key为变量名，fallback为默认十进制值，maximum为允许最大值。
// 返回值：合法正整数；越界或格式错误返回脱敏配置错误。
func boundedImportInt(key, fallback string, maximum int) (int, error) {
	value, err := strconv.Atoi(env(key, fallback))
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("%s must be between 1 and %d", key, maximum)
	}
	return value, nil
}
