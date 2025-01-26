package storage

import (
 "encoding/json"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "sync"

 "github.com/cespare/xxhash/v2"
    "distributed-db/internal/common"
)

type Storage struct {
 data      map[string][]byte
 mu        sync.RWMutex
 filePath string
 aesKey    string
 disableEncryption bool
}

type StorageData struct {
 Data map[string][]byte `json:"data"`
}

func NewStorage(filePath string, aesKey string, disableEncryption bool) *Storage {
 storage := &Storage{
  data:      make(map[string][]byte),
  filePath: filePath,
  aesKey: aesKey,
  disableEncryption: disableEncryption,
 }
    storage.loadDataFromDisk()
 return storage
}

func (s *Storage) Set(key string, value []byte) {
 s.mu.Lock()
 defer s.mu.Unlock()

 s.data[key] = value
}

func (s *Storage) Get(key string) ([]byte, bool) {
 s.mu.RLock()
 defer s.mu.RUnlock()

 value, ok := s.data[key]
 return value, ok
}
func (s *Storage) Delete(key string) {
 s.mu.Lock()
 defer s.mu.Unlock()
    delete(s.data, key)
}

func (s *Storage) PersistDataToDisk() error {
 s.mu.RLock()
 defer s.mu.RUnlock()

 data := StorageData{
  Data: s.data,
 }

 file, err := os.CreateTemp(filepath.Dir(s.filePath), "tempdata")
    if err != nil {
        return fmt.Errorf("failed to create temporary file: %w", err)
    }
 defer file.Close()

 var jsonData []byte
    if s.disableEncryption || s.aesKey == "" {
         jsonData, err = json.Marshal(data)
  if err != nil {
   return fmt.Errorf("failed to marshal data: %w", err)
  }
    }else{
        jsonData, err = s.encryptData(data)
  if err != nil {
   return fmt.Errorf("failed to marshal data: %w", err)
  }
    }
    _, err = file.Write(jsonData)
  if err != nil {
   return fmt.Errorf("failed to write data to temp file: %w", err)
  }

    err = os.Rename(file.Name(), s.filePath)
    if err != nil {
        return fmt.Errorf("failed to move temp file to final destination: %w", err)
    }

    return nil
}

func (s *Storage) loadDataFromDisk() {
    s.mu.Lock()
 defer s.mu.Unlock()

    file, err := os.Open(s.filePath)
    if os.IsNotExist(err){
        return
    }
    if err != nil {
        fmt.Printf("failed to open data file: %v\n", err)
        return
    }
    defer file.Close()

    fileInfo, err := file.Stat()
    if err != nil{
        fmt.Printf("failed to get file info: %v\n", err)
        return
    }

    if fileInfo.Size() == 0 {
        return
    }

    jsonData, err := io.ReadAll(file)
    if err != nil {
  fmt.Printf("failed to read data from file: %v\n", err)
        return
    }
    var storageData StorageData
    if s.disableEncryption || s.aesKey == ""{
        err = json.Unmarshal(jsonData, &storageData)
         if err != nil {
   fmt.Printf("failed to unmarshal data: %v\n", err)
            return
  }
    }else{
        err = s.decryptData(jsonData, &storageData)
        if err != nil {
   fmt.Printf("failed to unmarshal data: %v\n", err)
            return
        }
    }

 s.data = storageData.Data
}

func (s *Storage) encryptData(data StorageData) ([]byte, error) {
 jsonData, err := json.Marshal(data)
 if err != nil {
  return nil, err
 }

 key := []byte(s.aesKey)

 encryptedData, err := common.EncryptAES(jsonData, key)
 if err != nil {
  return nil, fmt.Errorf("failed to encrypt data: %w", err)
 }

 return encryptedData, nil
}


func (s *Storage) decryptData(encryptedData []byte, data *StorageData) error {

    key := []byte(s.aesKey)

 decryptedData, err := common.DecryptAES(encryptedData, key)
 if err != nil {
  return  fmt.Errorf("failed to decrypt data: %w", err)
 }

 err = json.Unmarshal(decryptedData, data)
 if err != nil {
  return fmt.Errorf("failed to unmarshal data: %w", err)
 }

 return nil
}
func (s *Storage) GetKeyHash(key string) uint64 {
 return xxhash.Sum64String(key)
}