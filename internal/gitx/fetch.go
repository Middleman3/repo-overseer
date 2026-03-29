package gitx

// FetchOriginPrune runs git fetch origin --prune (updates remote-tracking refs and drops deleted ones).
func FetchOriginPrune(repo string) error {
	cmd := gitCmd(repo, "fetch", "origin", "--prune")
	return cmd.Run()
}
