package cache

import (
	"fmt"
	"hash/fnv"
	"io"
)

func Key(gen uint64, sqlText string, args []any) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00", gen)
	io.WriteString(h, sqlText)
	h.Write([]byte{0})
	for _, a := range args {
		fmt.Fprintf(h, "%T:%v\x00", a, a)
	}
	return h.Sum64()
}
