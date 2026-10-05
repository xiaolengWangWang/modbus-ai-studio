//go:build !darwin

package update

func installDMG(string, string) error { return ErrUnsupported }
