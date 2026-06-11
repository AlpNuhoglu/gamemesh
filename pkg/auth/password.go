package auth

import "golang.org/x/crypto/bcrypt"

// bcryptCost 12 is a deliberate step above the library default (10): login is
// rare relative to gameplay traffic, so we can afford ~250ms of hashing for
// materially better resistance to offline cracking.
const bcryptCost = 12

// HashPassword hashes a plaintext password with bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(hash), err
}

// CheckPassword reports whether the plaintext matches the stored hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
