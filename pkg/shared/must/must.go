// Package must turns an error that cannot happen into a panic, so that a caller does not carry a
// branch no test can reach. Use it only for work that cannot fail with the inputs the program
// itself chose: generating a key from the system's random source, signing with a key just made,
// parsing a certificate just built. Never use it for anything that depends on a person, a file, the
// network or a database.
package must

// Do panics when err is not nil.
func Do(err error) {
	if err != nil {
		panic(err)
	}
}

// Value returns v, and panics when err is not nil.
func Value[T any](v T, err error) T {
	Do(err)
	return v
}

// Value2 returns a and b, and panics when err is not nil.
func Value2[A, B any](a A, b B, err error) (A, B) {
	Do(err)
	return a, b
}
