package modelregistry

func routingInt(value int) *int { return &value }
func withModel(selection Selection, modelID string) Selection {
	selection.ModelID = modelID
	return selection
}
