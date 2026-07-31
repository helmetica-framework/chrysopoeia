package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

func Test_InjectedLabelValidation(t *testing.T) {
	req, err := labels.NewRequirement("some.valid/label", selection.Exists, nil)
	if assert.NoError(t, err) {
		assert.Equal(t, "some.valid/label", req.String())
	}

	_, err = labels.NewRequirement("!cheater", selection.Exists, nil)
	assert.Error(t, err)
}
