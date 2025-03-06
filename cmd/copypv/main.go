package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rkfg/authproxy/civitai"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Println("Provide source and destination directories to copy the previews")
		os.Exit(1)
	}
	src := os.Args[1]
	dst := os.Args[2]
	_ = dst
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetName := filepath.Join(dst, rel)
		if d.IsDir() {
			err = os.MkdirAll(targetName, 0755)
			return err
		}
		if strings.HasSuffix(d.Name(), ".preview.png") || strings.HasSuffix(d.Name(), ".preview.mp4") {
			fmt.Printf("Linking/copying %s to %s\n", path, targetName)
			return civitai.CopyLink(path, targetName)
		}
		return nil
	})
	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
