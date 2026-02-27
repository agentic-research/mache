package main

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "app",
	Short: "A brief description of your application",
}

var toggle string

func init() {
	rootCmd.Flags().StringVarP(&toggle, "toggle", "t", "false", "Help message for toggle")
}
