// Package authz exposes the contract application's versioned authorization manifest.
package authz

import _ "embed"

// PermissionManifest contains the application-owned role and permission catalog.
//
//go:embed permission-manifest.yaml
var PermissionManifest []byte
