//go:build !linux

package observability

import "errors"

var errNoCgroupV2 = errors.New("observability: cgroup v2 is linux-only")

func CurrentCgroup() (string, error)                      { return "", errNoCgroupV2 }
func CreateSubCgroup(parent, name string) (string, error) { return "", errNoCgroupV2 }
func RemoveCgroup(_ string) error                         { return errNoCgroupV2 }
func OpenCgroupFD(_ string) (int, error)                  { return -1, errNoCgroupV2 }
func DelegationOK() bool                                  { return false }
func collectCgroupSample(_ string) (*CgroupSample, error) { return nil, errNoCgroupV2 }
func SwapCgroupRoot(_ string) string                      { return "" }
