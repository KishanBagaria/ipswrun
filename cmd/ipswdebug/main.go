// TEMPORARY: lists directories of a restore image's system volume, to see where iOS 12 and 13 keep
// their shared cache.
package main

import (
	"fmt"
	"os"

	"github.com/KishanBagaria/ipswrun/rangefs"
)

func main() {
	image, err := rangefs.Open(os.Args[2], os.TempDir()+"/ipswdebug", os.Args[1])
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	for _, dir := range os.Args[3:] {
		names, err := image.ReadDir(rangefs.FileSystem, dir)
		fmt.Printf("%s: %v %v\n", dir, names, err)
		for _, n := range names {
			e, err := image.Stat(rangefs.FileSystem, dir+"/"+n)
			size := int64(-1)
			if e.File != nil {
				size = e.File.Size()
			}
			fmt.Printf("  %s kind=%v target=%q size=%d err=%v\n", n, e.Kind, e.Target, size, err)
		}
	}
}
