package web

import "golang.org/x/crypto/bcrypt"

// The tests sign up many accounts; the production work factor would make them slow.
func init() { bcryptCost = bcrypt.MinCost }
