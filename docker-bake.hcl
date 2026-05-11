variable "TAG" {
  default = "latest"
}

variable "REGISTRY" {
  default = "ghcr.io/tigrisdata-community/videosite"
}

group "default" {
  targets = ["www", "encoder"]
}

target "www" {
  context    = "."
  dockerfile = "docker/www.Dockerfile"
  platforms  = ["linux/amd64"]
  tags       = ["${REGISTRY}/www:${TAG}"]
}

target "encoder" {
  context    = "."
  dockerfile = "docker/encoder.Dockerfile"
  platforms  = ["linux/amd64"]
  tags       = ["${REGISTRY}/encoder:${TAG}"]
}
