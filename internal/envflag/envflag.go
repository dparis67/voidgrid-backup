// Package envflag registers command-line flags whose defaults can come from
// environment variables, so the same setting works as -flag or in a .env file.
// A flag given on the command line always wins over the environment.
package envflag

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// String registers a string flag defaulting to $env when set, else def.
func String(name, env, def, usage string) *string {
	if v, ok := os.LookupEnv(env); ok {
		def = v
	}
	return flag.String(name, def, usage+" [$"+env+"]")
}

// Duration registers a duration flag defaulting to $env when set, else def.
// An unparsable $env is a startup error rather than a silent fallback.
func Duration(name, env string, def time.Duration, usage string) *time.Duration {
	if v, ok := os.LookupEnv(env); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid $%s=%q: %v\n", env, v, err)
			os.Exit(2)
		}
		def = d
	}
	return flag.Duration(name, def, usage+" [$"+env+"]")
}

// Bool registers a bool flag defaulting to $env when set, else def. An
// unparsable $env is a startup error rather than a silent fallback.
func Bool(name, env string, def bool, usage string) *bool {
	if v, ok := os.LookupEnv(env); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid $%s=%q: %v\n", env, v, err)
			os.Exit(2)
		}
		def = b
	}
	return flag.Bool(name, def, usage+" [$"+env+"]")
}

// Int registers an int flag defaulting to $env when set, else def. An
// unparsable $env is a startup error rather than a silent fallback.
func Int(name, env string, def int, usage string) *int {
	if v, ok := os.LookupEnv(env); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid $%s=%q: %v\n", env, v, err)
			os.Exit(2)
		}
		def = n
	}
	return flag.Int(name, def, usage+" [$"+env+"]")
}
