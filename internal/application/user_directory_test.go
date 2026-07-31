package application

import "testing"

func TestUserDirectoryReturnsOnlyNamedUsersSortedByChineseName(t *testing.T) {
	service := Service{UserDisplayNames: map[string]string{
		"user-3": "",
		"user-2": "章六",
		"user-1": "蔡总",
	}}

	got := service.UserDirectory()

	if len(got) != 2 || got[0].UserID != "user-2" || got[0].DisplayName != "章六" ||
		got[1].UserID != "user-1" || got[1].DisplayName != "蔡总" {
		t.Fatalf("UserDirectory() = %#v", got)
	}
}
