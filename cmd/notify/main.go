// Command notify is the entrypoint for the notify platform binary.
package main

import (
	"fmt"

	"github.com/danmestas/notify/internal/version"
)

func main() {
	fmt.Printf("notify %s\n", version.Version)
}
