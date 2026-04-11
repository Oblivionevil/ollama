//go:build windows || darwin

package assets

import (
	"embed"
)

//go:embed *.ico
var icons embed.FS

func GetIcon(filename string) ([]byte, error) {
	return icons.ReadFile(filename)
}
