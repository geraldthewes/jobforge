package unit

import (
	"testing"

	"nomad-mcp-builder/pkg/types"
)

// TestPublishedImageType tests the PublishedImage type structure
func TestPublishedImageType(t *testing.T) {
	tests := []struct {
		name          string
		image         types.PublishedImage
		expectedName  string
		expectedSize  int64
		expectedHuman string
	}{
		{
			name: "basic image metadata",
			image: types.PublishedImage{
				Name:      "registry.cluster:5000/myapp:v1.0.0",
				SizeBytes: 125000000,
				SizeHuman: "119.2 MB",
			},
			expectedName:  "registry.cluster:5000/myapp:v1.0.0",
			expectedSize:  125000000,
			expectedHuman: "119.2 MB",
		},
		{
			name: "small image",
			image: types.PublishedImage{
				Name:      "registry.cluster:5000/tiny:latest",
				SizeBytes: 1024,
				SizeHuman: "1.0 KB",
			},
			expectedName:  "registry.cluster:5000/tiny:latest",
			expectedSize:  1024,
			expectedHuman: "1.0 KB",
		},
		{
			name: "large image",
			image: types.PublishedImage{
				Name:      "registry.cluster:5000/large:latest",
				SizeBytes: 2147483648, // 2 GB
				SizeHuman: "2.0 GB",
			},
			expectedName:  "registry.cluster:5000/large:latest",
			expectedSize:  2147483648,
			expectedHuman: "2.0 GB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.image.Name != tt.expectedName {
				t.Errorf("Name = %v, want %v", tt.image.Name, tt.expectedName)
			}
			if tt.image.SizeBytes != tt.expectedSize {
				t.Errorf("SizeBytes = %v, want %v", tt.image.SizeBytes, tt.expectedSize)
			}
			if tt.image.SizeHuman != tt.expectedHuman {
				t.Errorf("SizeHuman = %v, want %v", tt.image.SizeHuman, tt.expectedHuman)
			}
		})
	}
}

// TestJobMetricsWithPublishedImages tests that JobMetrics includes PublishedImages
func TestJobMetricsWithPublishedImages(t *testing.T) {
	metrics := types.JobMetrics{
		PublishedImages: []types.PublishedImage{
			{
				Name:      "registry.cluster:5000/app:v1.0.0",
				SizeBytes: 100000000,
				SizeHuman: "95.4 MB",
			},
			{
				Name:      "registry.cluster:5000/app:latest",
				SizeBytes: 100000000,
				SizeHuman: "95.4 MB",
			},
		},
	}

	if len(metrics.PublishedImages) != 2 {
		t.Errorf("Expected 2 published images, got %d", len(metrics.PublishedImages))
	}

	// Verify first image
	if metrics.PublishedImages[0].Name != "registry.cluster:5000/app:v1.0.0" {
		t.Errorf("First image name = %v, want registry.cluster:5000/app:v1.0.0", metrics.PublishedImages[0].Name)
	}

	// Verify second image
	if metrics.PublishedImages[1].Name != "registry.cluster:5000/app:latest" {
		t.Errorf("Second image name = %v, want registry.cluster:5000/app:latest", metrics.PublishedImages[1].Name)
	}
}

// TestJobMetricsEmptyPublishedImages tests JobMetrics with no published images
func TestJobMetricsEmptyPublishedImages(t *testing.T) {
	metrics := types.JobMetrics{}

	if metrics.PublishedImages != nil {
		t.Errorf("Expected PublishedImages to be nil by default, got %v", metrics.PublishedImages)
	}

	if len(metrics.PublishedImages) != 0 {
		t.Errorf("Expected 0 published images, got %d", len(metrics.PublishedImages))
	}
}
