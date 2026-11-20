// Research tool, not product code. sign <in> <out> <identity> <entitlement key> <string value>: ad hoc signs a Mach-O with one entitlement.
package main

import (
	"fmt"
	"os"

	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/pkg/codesign"
	"github.com/blacktop/go-macho/pkg/codesign/types"
)

func main() {
	f, err := macho.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	key, value := os.Args[4], os.Args[5]
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>` + key + `</key><string>` + value + `</string></dict></plist>`
	pair := append(append([]byte{0x0C, byte(len(key))}, key...), append([]byte{0x0C, byte(len(value))}, value...)...)
	seq := append([]byte{0x30, byte(len(pair))}, pair...)
	ctx := append([]byte{0xB0, byte(len(seq))}, seq...)
	der := append([]byte{0x70, byte(3 + len(ctx)), 0x02, 0x01, 0x01}, ctx...)
	if err := f.CodeSign(&codesign.Config{ID: os.Args[3], Flags: types.ADHOC, Entitlements: []byte(xml), EntitlementsDER: der}); err != nil {
		panic(err)
	}
	if err := f.Save(os.Args[2]); err != nil {
		panic(err)
	}
	fmt.Println("signed", os.Args[2])
}
