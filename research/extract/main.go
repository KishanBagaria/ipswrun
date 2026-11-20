// Research tool, not product code. extract <url> <cache> <out dir> <path>...: copies system-volume files out of a restore image.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/KishanBagaria/ipswrun/rangefs"
)

func main() {
	image, err := rangefs.Open(os.Args[1], os.Args[2], "Macmini9,1")
	if err != nil {
		panic(err)
	}
	for _, path := range os.Args[4:] {
		f, err := image.Open(rangefs.FileSystem, path)
		if err != nil || f == nil {
			fmt.Println(path, "missing", err)
			continue
		}
		data, err := f.ReadAll()
		if err != nil {
			panic(err)
		}
		out := filepath.Join(os.Args[3], filepath.Base(path))
		os.WriteFile(out, data, 0o755)
		fmt.Println(out, len(data))
	}
}
