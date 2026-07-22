package application

type StaticApprovers map[string][]string

func (s StaticApprovers) Resolve(roleCode string) []string {
	return append([]string(nil), s[roleCode]...)
}
