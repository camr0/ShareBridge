package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/db"
)

var dbPath string

func main() {
	rootCmd := &cobra.Command{
		Use:   "admin",
		Short: "ShareBridge signaling server admin tool",
	}
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "./signaling.db", "database path")

	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(createKeyCmd)
	rootCmd.AddCommand(listKeysCmd)
	rootCmd.AddCommand(revokeKeyCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations",
	RunE: func(cmd *cobra.Command, args []string) error {
		database, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		fmt.Println("Migrations complete")
		return nil
	},
}

var createKeyCmd = &cobra.Command{
	Use:   "create-key",
	Short: "Create a new API key",
	RunE: func(cmd *cobra.Command, args []string) error {
		database, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()

		repo := db.NewAPIKeyRepo(database)

		// Generate key using shared functions from db package
		keyID := db.GenerateKeyID()
		secret := db.GenerateSecret()
		fullKey := keyID + "." + secret

		// Hash and store
		hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash key: %w", err)
		}

		if err := repo.Create(keyID, string(hash)); err != nil {
			return fmt.Errorf("store key: %w", err)
		}

		fmt.Println("API key created (copy now - will not be shown again):")
		fmt.Printf("  ID:       %s\n", keyID)
		fmt.Printf("  Full key: %s\n", fullKey)
		fmt.Println("\nSet environment variable:")
		fmt.Printf("  export SHAREBRIDGE_API_KEY=%s\n", fullKey)

		return nil
	},
}

var listKeysCmd = &cobra.Command{
	Use:   "list-keys",
	Short: "List all API keys",
	RunE: func(cmd *cobra.Command, args []string) error {
		database, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()

		repo := db.NewAPIKeyRepo(database)
		keys, err := repo.List()
		if err != nil {
			return fmt.Errorf("list keys: %w", err)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tCREATED AT\tLAST USED\tACTIVE")
		for _, k := range keys {
			lastUsed := "never"
			if k.LastUsedAt != nil {
				lastUsed = k.LastUsedAt.Format("2006-01-02")
			}
			active := "yes"
			if !k.IsActive {
				active = "no"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.ID, k.CreatedAt.Format("2006-01-02"), lastUsed, active)
		}
		w.Flush()

		return nil
	},
}

var revokeKeyCmd = &cobra.Command{
	Use:   "revoke-key [KEY_ID]",
	Short: "Revoke an API key",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		database, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()

		// Confirmation prompt
		fmt.Printf("Are you sure you want to revoke key %s? (yes/no): ", args[0])
		var response string
		fmt.Scanln(&response)
		if response != "yes" {
			fmt.Println("revocation cancelled")
			return nil
		}

		repo := db.NewAPIKeyRepo(database)
		found, err := repo.Revoke(args[0])
		if err != nil {
			return fmt.Errorf("revoke key: %w", err)
		}
		if !found {
			return fmt.Errorf("key not found: %s", args[0])
		}

		fmt.Printf("revoked: %s\n", args[0])
		return nil
	},
}
