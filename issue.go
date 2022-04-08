package main

type issueType int

const (
	warning issueType = iota
	danger
)

type issueEntry struct {
	issueType issueType
	message   string
}

type issueEntries []issueEntry

func (il issueEntries) messages() []string {
	messages := make([]string, len(il))
	for i, issue := range il {
		messages[i] = issue.message
	}
	return messages
}

func (il issueEntries) filter(t issueType) (filtered []string) {
	for _, issue := range il {
		if issue.issueType == t {
			filtered = append(filtered, issue.message)
		}
	}
	return
}
