package provider

import "context"

// ImageRequest asks an image model for one image.
type ImageRequest struct {
	Model  Model
	Prompt string
}

// ImageResponse is one generated image. Mime is derived from the bytes by the
// transport and is one of image/png, image/jpeg or image/webp — never the
// upstream's word alone.
type ImageResponse struct {
	Data  []byte
	Mime  string
	Usage Usage
}

// ImageGenerator is implemented by transports that serve image models
// (docs/models.md, "Image models"). Errors are *Error values. A reply that
// arrived but held no usable image comes back as an error together with a
// response holding only its Usage, so the caller can count the call.
type ImageGenerator interface {
	GenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
}
