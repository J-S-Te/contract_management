// Package authz 提供合同应用版本化的授权清单。
package authz

import _ "embed"

// PermissionManifest 包含应用自有的角色和权限目录。
//
//go:embed permission-manifest.yaml
var PermissionManifest []byte
