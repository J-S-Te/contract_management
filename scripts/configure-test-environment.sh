#!/usr/bin/env bash
set -Eeuo pipefail

environment_name="test"
deploy_host="47.111.20.119"

required_command() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "缺少命令：$1" >&2
    exit 1
  }
}

required_value() {
  local name="$1"
  local value="${!name:-}"
  [[ -n "$value" ]] || {
    echo "缺少环境变量：$name" >&2
    exit 1
  }
}

required_file() {
  local name="$1"
  local path="${!name:-}"
  [[ -f "$path" && -s "$path" ]] || {
    echo "$name 不是非空文件：$path" >&2
    exit 1
  }
}

required_command gh
required_command ssh-keygen

required_value DEPLOY_USER
required_value DEPLOY_SSH_KEY_FILE
required_value DEPLOY_KNOWN_HOSTS_FILE
required_file DEPLOY_SSH_KEY_FILE
required_file DEPLOY_KNOWN_HOSTS_FILE

deploy_port="${DEPLOY_PORT:-22}"
deploy_path="${DEPLOY_PATH:-/opt/basic-platform}"
repository="${GITHUB_REPOSITORY:-}"

if [[ ! "$deploy_port" =~ ^[0-9]+$ ]] || ((deploy_port < 1 || deploy_port > 65535)); then
  echo "DEPLOY_PORT 必须是 1-65535：$deploy_port" >&2
  exit 1
fi
if [[ "$deploy_path" != /* ]]; then
  echo "DEPLOY_PATH 必须是绝对路径：$deploy_path" >&2
  exit 1
fi
if [[ -z "$repository" ]]; then
  repository="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
fi
if [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "GITHUB_REPOSITORY 格式错误：$repository" >&2
  exit 1
fi

gh auth status >/dev/null
ssh-keygen -l -f "$DEPLOY_SSH_KEY_FILE" >/dev/null
ssh-keygen -l -f "$DEPLOY_KNOWN_HOSTS_FILE" >/dev/null

echo "创建或更新 GitHub Environment：$repository/$environment_name"
gh api --method PUT "repos/$repository/environments/$environment_name" >/dev/null

printf '%s' "$DEPLOY_USER" |
  gh secret set DEPLOY_USER --env "$environment_name" --repo "$repository"
printf '%s' "$deploy_port" |
  gh secret set DEPLOY_PORT --env "$environment_name" --repo "$repository"
gh secret set DEPLOY_SSH_KEY --env "$environment_name" --repo "$repository" \
  <"$DEPLOY_SSH_KEY_FILE"
gh secret set DEPLOY_KNOWN_HOSTS --env "$environment_name" --repo "$repository" \
  <"$DEPLOY_KNOWN_HOSTS_FILE"
gh variable set DEPLOY_PATH --env "$environment_name" --repo "$repository" \
  --body "$deploy_path"

echo "test Environment 已配置"
echo "部署主机由工作流固定为：$deploy_host"
echo "部署目录：$deploy_path"
echo "推送 main 后将自动部署；请确认 test Environment 未设置 Required reviewers。"
