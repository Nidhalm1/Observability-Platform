# One Dockerfile, three services. `--build-arg SERVICE=orders` picks which
# main package to compile, so the build stays identical across all three.
#
#   docker build --build-arg SERVICE=orders -t obs/orders .

FROM golang:1.22-alpine AS build
ARG SERVICE
WORKDIR /src

# Copy the manifests first so `go mod download` is cached independently of
# source changes -- otherwise every edit re-downloads the module graph.
# download dependecies that are called for this service. or project ( more project)
COPY go.mod go.sum ./  
RUN go mod download 

COPY . .  
#copy our project in folder named src/

# CGO_ENABLED=0 produces a static binary, which is what lets the final stage be
# distroless/static. A dynamically linked binary would need libc and force a
# much larger base image with OS packages -- i.e. CVEs for Trivy to find later.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/app ./services/${SERVICE}

# starts an image where it has only the excutable , not the go compiler 
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# /app is the excutable location inside the final image 
# put it in the image as /app
USER nonroot:nonroot
# note really impotant
EXPOSE 8080

#wich program to run when the container starts, in this case /app
ENTRYPOINT ["/app"]
