package vpkg

import "fmt"

var VortexErrors = struct {
	ErrUidIsEmpty error
}{
	ErrUidIsEmpty: fmt.Errorf("uid is empty"),
}
