package contract

import "time"

// ImportAutoApproveSetting 是历史导入自动审批的租户级运行开关。
//
// 生效条件是「env 能力开关开启且本记录 Enabled=true」：env 关闭时该设置不参与
// 判定、接口也不可用；env 开启但本记录不存在时按零值（Version=0、Enabled=false）
// 处理，历史导入仍走人工会签。Version 从 1 起单调递增，写入方必须提交
// expected_version 做乐观并发，UpdatedBy/UpdatedAt 只记录最近一次运行开关变更。
type ImportAutoApproveSetting struct {
	TenantID  string
	Enabled   bool
	Version   uint64
	UpdatedBy string
	UpdatedAt time.Time
}
