//go:build windows

package app

// ownerOf reports the owning user of a path for an error message.
// On Windows, inspecting POSIX ownership is not supported; return a generic name.
func ownerOf(path string) string {
	return "the database"
}
