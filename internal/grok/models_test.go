package grok

import (
	"testing"
)

func TestParseModelsPreservesNativeCatalogAndEfforts(t *testing.T) {
	catalog := `{"availableModels":[{"modelId":"grok-4.7","_meta":{"reasoningEffort":"high","reasoningEfforts":[{"value":"low"},{"value":"high","default":true},{"value":"xhigh"}]}},{"modelId":"future-grok","_meta":{"reasoningEfforts":[{"value":"medium","default":true}]}},{"modelId":"custom-model"}]}`
	for _, raw := range []string{catalog, `{"result":` + catalog + `}`, `{"data":` + catalog + `}`} {
		models, err := parseModels([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 3 || models[0].ID != "grok-4.7" || models[1].ID != "future-grok" {
			t.Fatalf("models = %+v", models)
		}
		if models[0].DefaultEffort != "high" || len(models[0].Efforts) != 3 || models[0].Efforts[2] != "xhigh" || models[1].DefaultEffort != "medium" {
			t.Fatalf("efforts = %+v", models)
		}
		if models[2].DefaultEffort != "" || len(models[2].Efforts) != 0 {
			t.Fatalf("invented efforts: %+v", models[2])
		}
	}
}
