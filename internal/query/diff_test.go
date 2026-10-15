package query

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDiff(t *testing.T) {
	diff := `diff --git a/service/user.go b/service/user.go
index 1111111..2222222 100644
--- a/service/user.go
+++ b/service/user.go
@@ -10,7 +10,8 @@ func LoadUser(ctx context.Context, id int64) (string, error) {
 	u, err := dal.QueryUser(ctx, id)
 	if err != nil {
-		return "", err
+		return "", fmt.Errorf("load user: %w", err)
 	}
+	// trimmed
 	return u.Name, nil
 }

@@ -30,2 +31,0 @@ func Other() {
-	a := 1
-	b := 2
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package service
-
-func Gone() {}
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package service
+func Added() {}
\ No newline at end of file
`
	ch, err := ParseDiff(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{
		"service/user.go": {12, 12, 14, 31, 31},
		"new.go":          {1, 2},
	}
	if !reflect.DeepEqual(ch.Lines, want) {
		t.Errorf("lines = %v, want %v", ch.Lines, want)
	}
	if !reflect.DeepEqual(ch.Deleted, []string{"old.go"}) {
		t.Errorf("deleted = %v", ch.Deleted)
	}
}

func TestMatchScore(t *testing.T) {
	name := "(*biz/service.OrderService).Create"
	if matchScore(name, name) != 4 || matchScore(name, "Create") != 3 ||
		matchScore(name, "OrderService).Create") != 2 || matchScore(name, "service.Order") != 1 ||
		matchScore(name, "Cancel") != 0 {
		t.Error("unexpected match scores")
	}
}
