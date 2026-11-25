package client

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func defaultClusterFilePath() (string, error) {
	// C++ uses SHGetFolderPath(CSIDL_COMMON_APPDATA), not the user's AppData.
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "foundationdb", "fdb.cluster"), nil
}
