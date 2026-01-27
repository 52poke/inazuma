package cache

import "fmt"

func PageKey(variant, title string) string {
	return fmt.Sprintf("page/%s/%s", variant, title)
}
