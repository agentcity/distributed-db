package data

import (
 "crypto/aes"
 "crypto/cipher"
 "crypto/rand"
 "encoding/json"
 "errors"
 "io"
 "os"
 "sync"
)

var ErrKeyExists = errors.New("key already exists")

type HashTable struct {
 data     map[string]bool
 mu       sync.RWMutex
 filename string
 key      []byte // Ключ шифрования
}

func NewHashTable(key string) *HashTable {
 return &HashTable{
  data: make(map[string]bool),
  mu:   sync.RWMutex{},
  key:  []byte(key), // Convert key to byte slice
 }
}

func (ht *HashTable) Put(key string) error {
 ht.mu.Lock()
 defer ht.mu.Unlock()
 if _, exists := ht.data[key]; exists {
  return ErrKeyExists
 }
 ht.data[key] = true
 return nil
}

func (ht *HashTable) Get(key string) bool {
 ht.mu.RLock()
 defer ht.mu.RUnlock()
 return ht.data[key]
}

func (ht *HashTable) Delete(key string) {
 ht.mu.Lock()
 defer ht.mu.Unlock()
 delete(ht.data, key)
}

func (ht *HashTable) SaveData(filename string) error {
 ht.mu.RLock()
 defer ht.mu.RUnlock()
 file, err := os.Create(filename)
 if err != nil {
  return err
 }
 defer file.Close()

 // Шифруем данные
 encryptedData, err := ht.encryptData()
 if err != nil {
  return err
 }

 encoder := json.NewEncoder(file)
 if err := encoder.Encode(encryptedData); err != nil {
  return err
 }
 return nil
}

func (ht *HashTable) LoadData(filename string) error {
 ht.filename = filename
 file, err := os.Open(filename)
 if err != nil {
  if os.IsNotExist(err) {
   return nil // Файл не существует, это нормально
  }
  return err
 }
 defer file.Close()

 var encryptedData []byte
 decoder := json.NewDecoder(file)
 if err := decoder.Decode(&encryptedData); err != nil {
  return err
 }
 // Расшифровываем данные
 err = ht.decryptData(encryptedData)
 if err != nil {
  return err
 }
 return nil
}
// encryptData шифрует данные
func (ht *HashTable) encryptData() ([]byte, error) {
    plaintext, err := json.Marshal(ht.data)
    if err != nil {
        return nil, err
    }

    block, err := aes.NewCipher(ht.key)
    if err != nil {
        return nil, err
    }

    ciphertext := make([]byte, aes.BlockSize+len(plaintext))
    iv := ciphertext[:aes.BlockSize]
    if _, err := io.ReadFull(rand.Reader, iv); err != nil {
        return nil, err
    }

    stream := cipher.NewCFBEncrypter(block, iv)
    stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)
    return ciphertext, nil
}
// decryptData расшифровывает данные
func (ht *HashTable) decryptData(ciphertext []byte) error {
    block, err := aes.NewCipher(ht.key)
    if err != nil {
        return err
    }

    if len(ciphertext) < aes.BlockSize {
        return errors.New("ciphertext is too short")
    }
    iv := ciphertext[:aes.BlockSize]
    ciphertext = ciphertext[aes.BlockSize:]

    stream := cipher.NewCFBDecrypter(block, iv)
    plaintext := make([]byte, len(ciphertext))
    stream.XORKeyStream(plaintext, ciphertext)

    err = json.Unmarshal(plaintext, &ht.data)
 if err != nil {
  return err
 }
    return nil
}
