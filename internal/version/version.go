package version

// These values are overridden by GoReleaser ldflags for release builds.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
}

func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}
