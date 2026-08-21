// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// browseAt is where the file chooser lives. It sits under the settings because
// that is the only thing it is for, and because it is offered by exactly the
// servers that can save what it chooses.
const browseAt = proxiesAt + "/browse"

// browseLimit is how many entries one listing shows.
//
// A directory holding a hundred thousand files would otherwise build a page
// nobody can read out of a listing nobody asked for. What was left out is said
// rather than dropped quietly: a chooser that silently stops at a limit is one
// that hides the file somebody came for.
const browseLimit = 500

// listSuffixes is what a list of addresses is called on disk.
//
// Only these are offered. A chooser is for finding one file among many, and a
// listing that puts every binary on the machine beside it is one nobody can
// use — it is also a way of learning what a machine holds, which this page has
// no business helping with.
var listSuffixes = []string{".txt", ".list", ".csv"}

// programDir is the directory this program is in, which is as far up as the
// chooser goes.
//
// Where a program is, is something only the system can say. When it will not
// say, the working directory is used: it is where a program started by hand
// nearly always is, and a chooser that opened nowhere would be a page that does
// not work rather than one that shows a little less.
func programDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// browseView is one directory, as far as this page is concerned.
type browseView struct {
	page
	// At is the directory being shown, as it reads from the root — the program's
	// own directory. The reader is choosing among what was put beside the
	// program, so the program is where the path starts; the machine's own
	// arrangement above that is neither theirs to walk nor this page's to show.
	At string
	// Up is the directory above, empty at the root. It is a field rather than
	// something the template works out, because the root is the one place where
	// walking up has to stop and a template cannot say so.
	Up      string
	Folders []browseEntry
	Files   []browseEntry
	// Left is how many entries were not shown.
	Left int
	// Complaint is why this directory could not be read, if it could not. A
	// directory this machine will not open is not a fault in the program, and
	// saying so is the whole of what this page can do about it.
	Complaint string
}

// browseEntry is one thing in the listing.
type browseEntry struct {
	Name string
	// Path is where it is, absolute, so the link needs no memory of where the
	// reader came from.
	Path string
}

// browse lists one directory beside this program so a path can be chosen by
// clicking rather than typed.
//
// It reads nothing and changes nothing: it lists names. The file it chooses is
// read by the rotor on its own timer, which is why the path is what the settings
// keep — a file handed over by the browser would arrive as bytes with no path,
// and the timer would then be re-reading a copy that can never change.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	lang := s.rememberLang(w, r)

	root := s.browseRoot
	at := startFrom(r.URL.Query().Get("at"), s.savedListPath(), root)
	view := browseView{
		page: s.frame(lang, "browse.title", proxiesAt),
		At:   shownFrom(root, at),
		Up:   above(at, root),
	}

	entries, err := os.ReadDir(at)
	if err != nil {
		// The name of the directory is already on the page; repeating the whole
		// error would put this machine's paths into a line somebody screenshots.
		view.Complaint = lang.T("browse.unreadable")
		s.render(w, r, "browse.html", view)
		return
	}
	view.Folders, view.Files, view.Left = split(at, entries)
	s.render(w, r, "browse.html", view)
}

// startFrom settles which directory to show, and never leaves the root.
//
// The asked-for path is made absolute before anything is done with it, and it is
// the absolute one that is checked and used: a check on the path as it arrived
// would be a check on something other than what is read. Anything outside the
// root — a path typed into the address bar, a saved file that has since moved —
// is answered with the root itself rather than with a refusal, because the
// reader asked to choose a file and the root is where they choose it.
func startFrom(asked, saved, root string) string {
	if asked == "" {
		asked = filepath.Dir(saved)
	}
	abs, err := filepath.Abs(asked)
	if err != nil {
		// Abs fails only when the working directory cannot be read, and a chooser
		// that gave up there would be a page that never opens.
		abs = filepath.Clean(asked)
	}
	if !within(root, abs) {
		return root
	}
	return abs
}

// within reports whether a path is the root or something under it.
//
// It compares whole names rather than text: a directory beside the root whose
// name merely starts the same way is not inside it, and a prefix test would say
// it was.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// shownFrom is a path as the reader is shown it: from the root, which is where
// their choosing starts.
func shownFrom(root, at string) string {
	rel, err := filepath.Rel(root, at)
	if err != nil || rel == "." {
		return string(filepath.Separator)
	}
	return string(filepath.Separator) + filepath.ToSlash(rel)
}

// above is the directory one step up, and empty at the root.
//
// Walking up stops at the root rather than at the machine's own: this chooser
// is for what sits beside the program, and the way out of the top of it is the
// link back to the settings.
func above(at, root string) string {
	if !within(root, filepath.Dir(at)) {
		return ""
	}
	return filepath.Dir(at)
}

// split sorts a directory's entries into the folders to walk into and the files
// worth choosing, and reports how many were left out.
//
// Anything that is neither is passed over in silence. A device, a socket or a
// link to nowhere is not a list of addresses, and a chooser that offered them
// would be offering something that cannot be read.
func split(at string, entries []os.DirEntry) (folders, files []browseEntry, left int) {
	for _, e := range entries {
		item := browseEntry{Name: e.Name(), Path: filepath.Join(at, e.Name())}
		switch {
		case e.IsDir():
			folders = append(folders, item)
		case e.Type().IsRegular() && looksLikeAList(e.Name()):
			files = append(files, item)
		}
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	// The folders are kept whole where anything has to go: walking is how the
	// reader reaches the file, and a listing that dropped the way there would
	// strand them somewhere they cannot leave.
	if len(folders)+len(files) > browseLimit {
		room := max(browseLimit-len(folders), 0)
		if room < len(files) {
			left = len(files) - room
			files = files[:room]
		}
	}
	return folders, files, left
}

// looksLikeAList reports whether a name is what a list of addresses is called.
func looksLikeAList(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range listSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// savedListPath is the file the settings already name, so the chooser opens
// where the reader last left it rather than somewhere they have to walk back
// from every time.
func (s *Server) savedListPath() string {
	saved, _ := s.current()
	if saved.Proxy.Kind != "file" {
		return ""
	}
	return saved.Proxy.Location
}
