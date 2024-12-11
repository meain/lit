package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

const (
	dbPath         = ".litdb"
	embeddingModel = "nomic-embed-text"
	embeddingDim   = 768 // Typical dimension for nomic-embed-text
)

func initDatabase() (*sql.DB, error) {
	// Ensure sqlite-vec is loaded
	sqlite_vec.Auto()

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create virtual table for vector embeddings
	_, err = db.Exec(fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS documents USING vec0(
			rowid INTEGER PRIMARY KEY AUTOINCREMENT,
			filepath TEXT,
			content TEXT,
			embedding float[%d]
		)
	`, embeddingDim))
	if err != nil {
		return nil, fmt.Errorf("failed to create vec table: %v", err)
	}

	return db, nil
}

// Define a struct to hold the JSON data
type EmbeddingRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

func createEmbedding(ctx context.Context, text string) ([]float32, error) {
	// Create a new HTTP request
	data := EmbeddingRequest{
		Model:  embeddingModel,
		Prompt: text,
	}

	// Marshal the data to JSON
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:11434/api/embeddings", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Set the content type to JSON
	req.Header.Set("Content-Type", "application/json")

	// Send the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Check the status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Decode the response
	var embeddingResp struct {
		Embedding []float64 `json:"embedding"`
	}
	err = json.NewDecoder(resp.Body).Decode(&embeddingResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %v", err)
	}

	// Ensure the embedding is exactly the expected dimension
	if len(embeddingResp.Embedding) != embeddingDim {
		return nil, fmt.Errorf("unexpected embedding dimension: got %d, want %d",
			len(embeddingResp.Embedding), embeddingDim)
	}

	// Convert the embedding to float32
	float32Embedding := make([]float32, len(embeddingResp.Embedding))
	for i, f := range embeddingResp.Embedding {
		float32Embedding[i] = float32(f)
	}

	return float32Embedding, nil
}

func addDocument(ctx context.Context, db *sql.DB, filePath string) error {
	// Check if file is a text file
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file %q: %v", filePath, err)
	}
	if fileInfo.IsDir() {
		return nil
	}
	if !isTextFile(filePath) {
		return nil
	}

	// Read file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}

	// Generate embedding
	embedding, err := createEmbedding(ctx, string(content))
	if err != nil {
		return fmt.Errorf("failed to create embedding: %v", err)
	}

	// Serialize the embedding
	serializedEmbedding, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return fmt.Errorf("failed to serialize embedding: %v", err)
	}

	// Insert document with vector embedding
	_, err = db.Exec(
		"INSERT INTO documents(filepath, content, embedding) VALUES (?, ?, ?)",
		filePath, string(content), serializedEmbedding)
	if err != nil {
		return fmt.Errorf("failed to insert document: %v", err)
	}

	fmt.Printf("Document added: %s\n", filePath)
	return nil
}

func isTextFile(filePath string) bool {
	// Try to read the first 512 bytes of the file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	if len(data) == 0 {
		return true
	}
	isBinary := false
	for _, b := range data {
		if b == 0 {
			isBinary = true
			break
		}
		if b > 127 && !isPrintable(b) {
			isBinary = true
			break
		}
	}
	return !isBinary
}

func isPrintable(b byte) bool {
	// Most printable characters are in the range of 32 to 126 in ASCII
	// and in the range of 192 to 255 in ISO 8859-1
	return (b >= 32 && b <= 126) || (b >= 192 && b <= 255)
}

func searchDocuments(db *sql.DB, queryEmbedding []float32, limit int) error {
	// Serialize the query embedding
	serializedQuery, err := sqlite_vec.SerializeFloat32(queryEmbedding)
	if err != nil {
		return fmt.Errorf("failed to serialize query embedding: %v", err)
	}

	// Perform vector similarity search
	rows, err := db.Query(`
		SELECT 
			rowid, 
			filepath, 
			distance 
		FROM documents 
		WHERE embedding match ?
		ORDER BY distance 
		LIMIT ?
	`, serializedQuery, limit)
	if err != nil {
		return fmt.Errorf("search query failed: %v", err)
	}
	defer rows.Close()

	// Print results
	fmt.Println("Search Results:")
	var count int
	for rows.Next() {
		var rowid int
		var filepath string
		var distance float64

		if err := rows.Scan(&rowid, &filepath, &distance); err != nil {
			return fmt.Errorf("failed to scan row: %v", err)
		}

		count++
		fmt.Printf("%d. %s (%.4f)\n", count, filepath, distance)
	}

	if count == 0 {
		fmt.Println("No results found.")
	}

	return nil
}

func main() {
	// Check if at least one argument is provided
	if len(os.Args) < 2 {
		fmt.Println("Usage: ./lit <command> [args]")
		fmt.Println("Commands:")
		fmt.Println("  add <filepath>    - Add a document to the database")
		fmt.Println("  search <query>    - Search documents by query")
		os.Exit(1)
	}

	// Initialize database
	db, err := initDatabase()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Handle commands
	switch os.Args[1] {
	case "add":
		if len(os.Args) < 3 {
			log.Fatal("Usage: ./lit add <filepath>...")
		}

		// Process provided files/folders
		for _, arg := range os.Args[2:] {
			// Resolve absolute path
			filePath, err := filepath.Abs(arg)
			if err != nil {
				log.Printf("Failed to resolve file path %q: %v", arg, err)
				continue
			}

			// Add document if it's a file
			if fileInfo, err := os.Stat(filePath); err != nil {
				log.Printf("Failed to stat file %q: %v", arg, err)
			} else if !fileInfo.IsDir() {
				if err := addDocument(ctx, db, filePath); err != nil {
					log.Printf("Failed to add document %q: %v", arg, err)
				}
			} else {
				// Walk directory and add documents recursively
				err = filepath.WalkDir(filePath, func(path string, dirEntry fs.DirEntry, err error) error {
					if err != nil {
						log.Printf("Failed to walk directory %q: %v", arg, err)
						return err
					}

					if !dirEntry.IsDir() {
						if err := addDocument(ctx, db, path); err != nil {
							log.Printf("Failed to add document %q: %v", path, err)
						}
					}
					return nil
				})
				if err != nil {
					log.Printf("Failed to walk directory %q: %v", arg, err)
				}
			}
		}
	case "search":
		if len(os.Args) < 3 {
			log.Fatal("Usage: ./lit search <query>")
		}
		query := os.Args[2]

		// Generate embedding for search query
		queryEmbedding, err := createEmbedding(ctx, query)
		if err != nil {
			log.Fatalf("Failed to create query embedding: %v", err)
		}

		// Perform search
		if err := searchDocuments(db, queryEmbedding, 5); err != nil {
			log.Fatalf("Search failed: %v", err)
		}

	default:
		log.Fatalf("Unknown command: %s", os.Args[1])
	}
}
