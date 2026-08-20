package sandbox

import "os"

func execEnv() []string {
	return os.Environ()
}
