#!/usr/bin/env bash
set -euo pipefail

# 执行合同客户端→CRM真实路由→合同客户端的双向线协议校验。
# 参数：无；可用CRM_PROVISION_CONTRACT_FILE指定仅含合成数据的交换文件。
# 返回值：任何阶段失败立即返回非零；三个阶段均通过才成功。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workspace_root="$(dirname "$repo_root")"
wire_file="${CRM_PROVISION_CONTRACT_FILE:-$(mktemp "${TMPDIR:-/tmp}/crm-provision-contract.XXXXXX")}"
if [[ -z "${CRM_PROVISION_CONTRACT_FILE:-}" ]]; then
  trap 'rm -f "$wire_file"' EXIT
fi
export CRM_PROVISION_CONTRACT_FILE="$wire_file"
unset CONTRACT_TEST_DSN CRM_TEST_DSN SETTLEMENT_TEST_DSN BACKCHANNEL_TEST_DSN
export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
go_bin="${CONTRACT_TEST_GO_BIN:-go}"
cd "$repo_root"
CRM_PROVISION_CONTRACT_PHASE=export "$go_bin" test ./internal/integration/crm -run '^TestCRMImportProvisionCrossRepository$' -count=1 -v
cd "$workspace_root/customer_and_opportunity"
"$go_bin" test ./internal/modules/importprovision -run '^TestContractImportProvisionCrossRepository$' -count=1 -v
cd "$repo_root"
CRM_PROVISION_CONTRACT_PHASE=verify "$go_bin" test ./internal/integration/crm -run '^TestCRMImportProvisionCrossRepository$' -count=1 -v
