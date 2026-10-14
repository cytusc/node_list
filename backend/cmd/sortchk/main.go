// Command sortchk sorts its stdin lines with domain.CompareNumeric so the Go
// comparator can be diffed against JavaScript's localeCompare on real data.
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var in []string
	for sc.Scan() {
		in = append(in, sc.Text())
	}
	if err := sc.Err(); err != nil {
		panic(err)
	}
	sort.SliceStable(in, func(i, j int) bool { return domain.CompareNumeric(in[i], in[j]) < 0 })
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, s := range in {
		fmt.Fprintln(w, s)
	}
}
