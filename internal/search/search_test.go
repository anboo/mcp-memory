package search

import "testing"

func TestRRFMerge(t *testing.T) {
	vec := []Hit{
		{ID: "a", Score: 0.9, TimeCreated: 100},
		{ID: "b", Score: 0.8, TimeCreated: 200},
		{ID: "c", Score: 0.7, TimeCreated: 300},
	}
	fts := []Hit{
		{ID: "b", Score: 1.0, TimeCreated: 200}, // пересечение с vec
		{ID: "d", Score: 0.9, TimeCreated: 400}, // только в fts
	}

	merged := rrfMerge(vec, fts, 10)
	if len(merged) != 4 {
		t.Fatalf("merged = %d, want 4 (a,b,c,d, b без дубля)", len(merged))
	}

	// b в обоих списках -> скор выше остальных
	var bScore float64
	for _, h := range merged {
		if h.ID == "b" {
			bScore = h.Score
		}
	}
	if bScore <= 0 {
		t.Fatal("b не найден в merge")
	}
	for _, h := range merged {
		if h.ID != "b" && h.Score >= bScore {
			t.Fatalf("b должен иметь наибольший скор (в обоих списках): %+v", h)
		}
	}
}

func TestRRFMergeLimit(t *testing.T) {
	vec := make([]Hit, 10)
	for i := range vec {
		vec[i] = Hit{ID: string(rune('a' + i)), TimeCreated: int64(i)}
	}
	merged := rrfMerge(vec, nil, 5)
	if len(merged) != 5 {
		t.Fatalf("limit не применён: %d", len(merged))
	}
}

func TestRRFMergeEmpty(t *testing.T) {
	if got := rrfMerge(nil, nil, 10); len(got) != 0 {
		t.Fatalf("пустые списки должны давать пустой результат: %d", len(got))
	}
}

func TestRRFMergeTieByTime(t *testing.T) {
	// при равных скорах (ранг 1 в разных списках) сортировка по time_created
	vec := []Hit{{ID: "old", TimeCreated: 100}}
	fts := []Hit{{ID: "new", TimeCreated: 200}}
	merged := rrfMerge(vec, fts, 10)
	if len(merged) != 2 {
		t.Fatalf("merged = %d", len(merged))
	}
	if merged[0].ID != "new" {
		t.Fatalf("при равенстве скоров свежий должен быть выше: %+v", merged)
	}
}
