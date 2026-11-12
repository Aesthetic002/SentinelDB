package main

import (
	"fmt"
	"sort"

	"sentineldb/internal/txn"
)

// printSchedule runs the conflict-serializability test on a schedule typed by hand.
func printSchedule(s string) {
	ops, err := txn.ParseSchedule(s)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	res := txn.CheckConflictSerializable(ops)
	keys := make([][2]uint64, 0, len(res.Edges))
	for e := range res.Edges {
		keys = append(keys, e)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	fmt.Println("precedence graph:")
	if len(keys) == 0 {
		fmt.Println("  (no conflicts)")
	}
	for _, e := range keys {
		fmt.Printf("  T%d -> T%d   because %s\n", e[0], e[1], res.Edges[e])
	}
	if res.Serializable {
		fmt.Printf("conflict-serializable: equivalent to serial order %v\n", res.Order)
	} else {
		fmt.Printf("NOT conflict-serializable: the graph has cycle %v\n", res.Cycle)
	}
}
