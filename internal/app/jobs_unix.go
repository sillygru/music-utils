//go:build !windows

package app

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// ownerOf reports the owning user of a path for an error message, falling back
// to the numeric id when the name is not resolvable.
func ownerOf(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "the database"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "the database"
	}
	if name, found := userByUID(int(stat.Uid)); found {
		return name
	}
	return strconv.Itoa(int(stat.Uid))
}

// userByUID resolves a uid to a name, used only to make an error actionable.
func userByUID(uid int) (string, bool) {
	user, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", false
	}
	return user.Username, true
}
