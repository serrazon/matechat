package updater

import (
	"context"
	"fmt"

	"github.com/creativeprojects/go-selfupdate"
)

const githubRepo = "serrazon/matechat"

// CheckAndUpdate checks for a newer release and replaces the binary if found.
// Returns new version string, or "" if already up to date.
func CheckAndUpdate(currentVersion string) (string, error) {
	u, err := selfupdate.NewUpdater(selfupdate.Config{})
	if err != nil {
		return "", err
	}
	latest, found, err := u.DetectLatest(context.Background(), selfupdate.ParseSlug(githubRepo))
	if err != nil {
		return "", fmt.Errorf("detect latest: %w", err)
	}
	if !found {
		return "", fmt.Errorf("no releases found for %s", githubRepo)
	}
	if latest.LessOrEqual(currentVersion) {
		return "", nil
	}
	exe, err := selfupdate.ExecutablePath()
	if err != nil {
		return "", err
	}
	if err := u.UpdateTo(context.Background(), latest, exe); err != nil {
		return "", fmt.Errorf("apply update: %w", err)
	}
	return latest.Version(), nil
}
