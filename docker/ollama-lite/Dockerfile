# Lightweight Ollama — extracts binary from official image into Alpine
#
# NOTE: Docker still has to download the full official image (~1.7GB compressed)
# during build, even though the final image is small. For Raspberry Pi, native
# install is strongly recommended (see below).
#
# Build:   docker build -t ollama-lite .
# With model baked in:
#          docker build --build-arg MODEL=nomic-embed-text -t ollama-lite .
#
# Result:  ~200MB runtime image (vs 3.5GB official)
#
# Run:     docker run -d -p 11434:11434 -v ollama-data:/root/.ollama ollama-lite
#
# ---
# Raspberry Pi: Don't use this. Install natively instead (~425MB total):
#   curl -fsSL https://ollama.com/install.sh | sh
#   ollama pull nomic-embed-text
# ---

FROM ollama/ollama:latest AS official

# ---- Runtime: just Alpine + the binary ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=official /bin/ollama /bin/ollama

# Optional: pull a model during build (bakes it into the image)
ARG MODEL=
RUN if [ -n "$MODEL" ]; then \
        echo "Pre-pulling model: $MODEL" && \
        /bin/ollama serve &>/tmp/ollama.log & \
        sleep 5 && \
        /bin/ollama pull "$MODEL" && \
        kill %1 2>/dev/null; \
    fi

ENV OLLAMA_HOST=0.0.0.0

EXPOSE 11434
VOLUME ["/root/.ollama"]

ENTRYPOINT ["/bin/ollama"]
CMD ["serve"]
