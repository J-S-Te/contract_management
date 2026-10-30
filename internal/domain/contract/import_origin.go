package contract

// OperationalOrigin 判断记录能否沿既有普通业务链派生项目、财务及经营指标。
// 参数：c为权威合同行；历史接续须通过独立目标放行，不能混走v1。
// 返回值：兼容旧普通记录，未知或任何历史策略均为false。
func OperationalOrigin(c Contract) bool {
	if c.OriginImportItemID != "" && (c.OriginKind != "NORMAL" || c.ImportMode != ImportNormal) {
		return false
	}
	return (c.OriginKind == "" || c.OriginKind == "NORMAL") && (c.ImportMode == "" || c.ImportMode == ImportNormal) && c.ImportPolicy == nil
}
