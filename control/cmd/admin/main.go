package main

import (
	"fmt"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

var (
	dbPath   string
	email    string
	password string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "admin",
		Short: "ShareBridge signaling server admin tool",
	}
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "./pb_data", "PocketBase data directory path")

	// create-superuser command
	createSuperuserCmd := &cobra.Command{
		Use:   "create-superuser",
		Short: "Create a new PocketBase superuser (for scripted deploys)",
		RunE:  runCreateSuperuser,
	}
	createSuperuserCmd.Flags().StringVar(&email, "email", "", "Superuser email address (required)")
	createSuperuserCmd.Flags().StringVar(&password, "password", "", "Superuser password (required)")
	createSuperuserCmd.MarkFlagRequired("email")
	createSuperuserCmd.MarkFlagRequired("password")

	rootCmd.AddCommand(createSuperuserCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCreateSuperuser(cmd *cobra.Command, args []string) error {
	// Create PocketBase app without starting the server
	app := pocketbase.New()

	// Override the data directory
	os.Args = []string{os.Args[0], "--dir=" + dbPath}

	// Bootstrap initializes the app (creates data dir, opens db, loads settings)
	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("failed to bootstrap PocketBase: %w", err)
	}
	defer app.ResetBootstrapState()

	// Find the superusers collection
	superusersCol, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		return fmt.Errorf("superusers collection not found: %w", err)
	}

	// Check if superuser already exists
	existing, _ := app.FindAuthRecordByEmail(superusersCol, email)
	if existing != nil {
		return fmt.Errorf("superuser with email %s already exists", email)
	}

	// Create new superuser record
	record := core.NewRecord(superusersCol)
	record.SetEmail(email)
	record.SetPassword(password)

	// Save the record
	if err := app.Save(record); err != nil {
		return fmt.Errorf("failed to create superuser: %w", err)
	}

	fmt.Printf("Superuser created successfully: %s\n", email)
	return nil
}
