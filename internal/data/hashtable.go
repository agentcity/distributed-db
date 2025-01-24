package data

import (
 "encoding/json"
 "errors"
 "fmt"
 "io/ioutil"
 "os"
 "sync"
)

var (
 ErrKeyExists = errors.New("key already exists")
    ErrKeyNotFound = errors.New("key not found")
)
type HashTable struct {
 data map[string]bool
    mu  sync.RWMutex
}

func NewHashTable() *HashTable {
    return &HashTable{
        data: make(map[string]bool),
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
 _, exists := ht.data[key]
 return exists
}
func (ht *HashTable) Delete(key string) error {
     ht.mu.Lock()
    defer ht.mu.Unlock()
     if _, exists := ht.data[key]; !exists {
         return ErrKeyNotFound
     }
    delete(ht.data, key)
    return nil
}

func (ht *HashTable) LoadData(filename string) error {
    ht.mu.Lock()
    defer ht.mu.Unlock()
 file, err := os.OpenFile(filename, os.O_RDONLY|os.O_CREATE, 0644)
    if err != nil {
        return fmt.Errorf("error opening file: %w", err)
    }
 defer file.Close()

 data, err := ioutil.ReadAll(file)
 if err != nil {
  return fmt.Errorf("error reading file: %w", err)
 }
 if len(data) == 0 {
  return nil
 }

 err = json.Unmarshal(data, &ht.data)
 if err != nil {
  return fmt.Errorf("error unmarshaling data: %w", err)
 }
    return nil
}

func (ht *HashTable) SaveData(filename string) error {
    ht.mu.RLock()
    defer ht.mu.RUnlock()
 file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
    if err != nil {
        return fmt.Errorf("error opening file: %w", err)
    }
 defer file.Close()

    jsonData, err := json.Marshal(ht.data)
    if err != nil {
  return fmt.Errorf("error marshaling data: %w", err)
    }
    _, err = file.Write(jsonData)
    if err != nil {
       return fmt.Errorf("error writing data to file: %w", err)
    }

    return nil
}
