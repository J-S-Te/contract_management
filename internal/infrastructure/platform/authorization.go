package platform

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type compactIdentity struct {
	Subject, IdentityID, TenantID, PersonID string
}

func principalFromAuthorizationContext(identity compactIdentity, context AuthorizationContext, catalog AuthorizationCatalog, clientID, applicationCode, environmentCode string) (application.Principal, error) {
	if context.Subject != identity.Subject || context.IdentityID != identity.IdentityID || context.TenantID != identity.TenantID ||
		context.ClientID != clientID || context.ApplicationCode != applicationCode || context.EnvironmentCode != environmentCode ||
		context.AuthorizationRevision == 0 {
		return application.Principal{}, fmt.Errorf("%w: identity, client or environment binding mismatch", ErrAuthorizationInvalid)
	}
	roles, err := validateKnownSet(context.Roles, catalog.Roles, "role")
	if err != nil || len(roles) == 0 {
		if err == nil {
			err = errors.New("role set is empty")
		}
		return application.Principal{}, fmt.Errorf("%w: %v", ErrAuthorizationForbidden, err)
	}
	permissionValues, err := validateKnownSet(context.Permissions, catalog.Permissions, "permission")
	if err != nil || len(permissionValues) == 0 {
		if err == nil {
			err = errors.New("permission set is empty")
		}
		return application.Principal{}, fmt.Errorf("%w: %v", ErrAuthorizationForbidden, err)
	}
	permissions := make(map[string]bool, len(permissionValues))
	for _, permission := range permissionValues {
		permissions[permission] = true
	}
	roleSet := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		roleSet[role] = struct{}{}
	}
	scopes, permissionScopes, err := validateDataScopes(context.DataScopes, roleSet, catalog, permissionValues, identity, environmentCode)
	if err != nil {
		return application.Principal{}, fmt.Errorf("%w: %v", ErrAuthorizationForbidden, err)
	}
	return application.Principal{
		Subject: identity.Subject, TenantID: identity.TenantID, UserID: identity.IdentityID, IdentityID: identity.IdentityID,
		PersonID: firstNonEmpty(context.PersonID, identity.PersonID), Roles: roles, Permissions: permissions,
		DataScopes: scopes, PermissionScopes: permissionScopes, AuthorizationRevision: context.AuthorizationRevision,
		CatalogVersion: catalog.Version,
	}, nil
}

func validateKnownSet(values []string, known map[string]struct{}, kind string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || value == "all" {
			return nil, fmt.Errorf("%s is empty, non-canonical or wildcard", kind)
		}
		if _, ok := known[value]; !ok {
			return nil, fmt.Errorf("unknown %s %q", kind, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("duplicate %s %q", kind, value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validateDataScopes(input []AuthorizationDataScope, roles map[string]struct{}, catalog AuthorizationCatalog, permissions []string, identity compactIdentity, environmentCode string) ([]application.DataScope, map[string]contract.ScopeFilter, error) {
	if len(input) == 0 {
		return nil, nil, errors.New("data scope set is empty")
	}
	permissionScopes := make(map[string]contract.ScopeFilter, len(permissions))
	permissionSet := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		permissionSet[permission] = struct{}{}
	}
	result := make([]application.DataScope, 0, len(input))
	seen := map[string]struct{}{}
	for _, raw := range input {
		raw.RoleCode = strings.TrimSpace(raw.RoleCode)
		raw.ScopeType = strings.ToUpper(strings.TrimSpace(raw.ScopeType))
		raw.ScopeID = strings.TrimSpace(raw.ScopeID)
		raw.EnvironmentCode = strings.TrimSpace(raw.EnvironmentCode)
		if raw.RoleCode == "" {
			return nil, nil, errors.New("data scope role_code is empty")
		}
		if _, ok := roles[raw.RoleCode]; !ok {
			return nil, nil, fmt.Errorf("data scope refers to inactive role %q", raw.RoleCode)
		}
		rolePermissions := catalog.RolePermissions[raw.RoleCode]
		if len(rolePermissions) == 0 {
			return nil, nil, fmt.Errorf("data scope role %q has no local permissions", raw.RoleCode)
		}
		switch raw.ScopeType {
		case "APPLICATION", "TENANT":
			if raw.EnvironmentCode != "" {
				return nil, nil, fmt.Errorf("%s scope must not carry environment_code", raw.ScopeType)
			}
			// The platform intentionally emits APPLICATION with an empty scope_id for
			// tenant-wide application access. TENANT is accepted for compatibility.
		case "ENVIRONMENT":
			if raw.EnvironmentCode != environmentCode || raw.ScopeID == "" {
				return nil, nil, errors.New("environment scope does not bind the configured environment")
			}
		case "SELF":
			if raw.ScopeID == "" || raw.ScopeID != identity.IdentityID || raw.EnvironmentCode != "" && raw.EnvironmentCode != environmentCode {
				return nil, nil, errors.New("self scope does not bind the current identity or environment")
			}
		case "ORG", "PROJECT":
			if raw.ScopeID == "" || raw.EnvironmentCode != "" && raw.EnvironmentCode != environmentCode {
				return nil, nil, fmt.Errorf("%s scope is missing its business identifier or has the wrong environment", raw.ScopeType)
			}
		default:
			return nil, nil, fmt.Errorf("unknown data scope type %q", raw.ScopeType)
		}
		key := raw.RoleCode + "\x00" + raw.ScopeType + "\x00" + raw.ScopeID + "\x00" + raw.EnvironmentCode
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, errors.New("duplicate data scope")
		}
		seen[key] = struct{}{}
		result = append(result, application.DataScope{RoleCode: raw.RoleCode, ScopeType: raw.ScopeType, ScopeID: raw.ScopeID, EnvironmentCode: raw.EnvironmentCode})
		for permission := range rolePermissions {
			if _, granted := permissionSet[permission]; !granted {
				continue
			}
			filter := permissionScopes[permission]
			switch raw.ScopeType {
			case "APPLICATION", "TENANT", "ENVIRONMENT":
				filter.AllowAll = true
			case "SELF":
				filter.AllowSelf = true
			case "ORG":
				filter.OrganizationIDs = appendUnique(filter.OrganizationIDs, raw.ScopeID)
			case "PROJECT":
				filter.ProjectIDs = appendUnique(filter.ProjectIDs, raw.ScopeID)
			}
			permissionScopes[permission] = filter
		}
	}
	for _, permission := range permissions {
		filter, ok := permissionScopes[permission]
		if !ok || !(filter.AllowAll || filter.AllowSelf || len(filter.OrganizationIDs) > 0 || len(filter.ProjectIDs) > 0) {
			return nil, nil, fmt.Errorf("permission %q has no applicable data scope", permission)
		}
		sort.Strings(filter.OrganizationIDs)
		sort.Strings(filter.ProjectIDs)
		permissionScopes[permission] = filter
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		return left.RoleCode+left.ScopeType+left.ScopeID+left.EnvironmentCode < right.RoleCode+right.ScopeType+right.ScopeID+right.EnvironmentCode
	})
	return result, permissionScopes, nil
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
