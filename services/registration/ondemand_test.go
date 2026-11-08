package registration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"steki-bot/internal/bot"
)

// TestCatchUpWorkStartsOnlyFromCommands guards a rule of this bot: work that
// repairs or refreshes what is already in Discord (reconciling registrations,
// redrawing the stickies, scanning old history, editing old announcements)
// runs when a moderator asks, never because a deploy, a reconnect or a timer
// did. Those passes read whole member lists or touch a message in every
// channel, so they must not fire on every start.
//
// Each function below may be mentioned only by a command handler (cmd*) or by
// another function on the list. Wiring one to a gateway event or a goLoop makes
// this fail.
func TestCatchUpWorkStartsOnlyFromCommands(t *testing.T) {
	onDemand := map[string]bool{
		"reconcile": true,
		"sweepHelp": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			allowed := strings.HasPrefix(fn.Name.Name, "cmd") || onDemand[fn.Name.Name]
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// A selector catches a call and a method value handed to goLoop alike.
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !onDemand[sel.Sel.Name] {
					return true
				}
				seen[sel.Sel.Name] = true
				if !allowed {
					t.Errorf("%s: %s uses %s, which may only start from a command",
						fset.Position(sel.Pos()), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}

	// A renamed function would leave the guard checking nothing.
	for name := range onDemand {
		if !seen[name] {
			t.Errorf("%s is never used; rename it here or drop it from the list", name)
		}
	}
}

func TestReconcileReportSummary(t *testing.T) {
	quiet := reconcileReport{Members: 812}
	if quiet.changed() != 0 {
		t.Fatalf("an untouched pass reports %d changes", quiet.changed())
	}
	got := quiet.summary(nil)
	if !strings.Contains(got, "812") || !strings.Contains(got, "ήδη σωστά") {
		t.Errorf("a pass with nothing to do should say so, got %q", got)
	}

	busy := reconcileReport{Members: 812, Closed: 3, Recreated: 2, Orphans: 1, Duplicates: 1, Adopted: 4, Waiting: 7}
	if busy.changed() != 11 {
		t.Errorf("changed = %d, want 11", busy.changed())
	}
	got = busy.summary(nil)
	for _, want := range []string{"Εγγραφές που έκλεισαν", "φτιάχτηκαν", "ξαναδέθηκαν", "χωρίς εγγραφή", "Διπλά", "7 μέλη περιμένουν"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ήδη σωστά") {
		t.Errorf("a pass that changed things claims all was right:\n%s", got)
	}

	// Members waiting for a channel are work left, even when nothing else changed.
	if got := (reconcileReport{Members: 5, Waiting: 2}).summary(nil); strings.Contains(got, "ήδη σωστά") {
		t.Errorf("a pass that left members waiting claims all was right:\n%s", got)
	}

	failed := reconcileReport{Members: 10, Closed: 1}.summary(bot.ErrGuildUnknown)
	if !strings.Contains(failed, "σταμάτησε") || !strings.Contains(failed, bot.ErrGuildUnknown.Error()) || strings.Contains(failed, "✅") {
		t.Errorf("a pass that stopped early must say why and not claim success:\n%s", failed)
	}
}
