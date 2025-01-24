package auth

import (
 "crypto/sha256"
 "encoding/hex"
 "fmt"

 "golang.org/x/crypto/bcrypt"
)

// HashPassword hashes the password using bcrypt.
func HashPassword(password string) string {
 hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
 if err != nil {
  fmt.Println("Error during password hashing")
  return ""
 }
 return string(hashedPassword)
}

// CheckPassword checks if the provided password matches the stored hash.
func CheckPassword(hashedPassword, password string) bool {
 err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
    return err == nil
}

func HashSHA256(data string) string {
 hasher := sha256.New()
 hasher.Write([]byte(data))
 hashedBytes := hasher.Sum(nil)
 return hex.EncodeToString(hashedBytes)
}
