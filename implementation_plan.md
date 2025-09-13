# Implementation Plan: Automatic, Zero-Downtime Nexus Proxy

## 1. High-Level Goal

To enable a "one-click" setup for the Nexus Proxy server on a standard Linux VM. The server will automatically acquire and renew its own TLS certificate with zero downtime, making it incredibly easy for users to deploy.

## 2. Core Feature: Automatic Zero-Downtime TLS

*   **Configuration:** The `config.yaml` will be simplified.
    *   A new `hubPublicHostname` field will specify the server's public domain (e.g., `nexus.example.com`).
    *   The existing `hubTlsCertFile` and `hubTlsKeyFile` fields will remain for users who prefer manual configuration, but will be mutually exclusive with `hubPublicHostname`.
    *   A new optional `acmeCacheDir` field will specify where to store the TLS certificates. If omitted, it will default to a directory named `acme_certs` created in the current working directory.
*   **Zero-Downtime Challenge Handling:** The server will handle ACME challenges without interrupting its primary proxying function. The Go `autocert` library will be used. Certificate renewals will happen automatically and be loaded into memory for new connections without requiring a server restart.
    *   **`TLS-ALPN-01` (Port 443):** The L4 proxy logic will be extended. When it inspects an incoming TLS connection, if the SNI hostname matches the proxy's own `hubPublicHostname`, it will internally route the connection to the `autocert` manager to solve the challenge. All other connections will be proxied to their backends as normal.
    *   **`HTTP-01` (Port 80):** The L7 proxy logic will be extended. When it inspects an incoming HTTP request, if the `Host` header matches the proxy's `hubPublicHostname`, it will internally route the request to the `autocert` manager. All other requests will be proxied to their backends as normal.

## 3. Distribution: Pre-Compiled Binaries via GitHub

*   A GitHub Actions workflow (free for public repos) will be created to automatically build and package the Nexus Proxy server upon every new release.
*   Binaries will be provided for `linux/amd64` and `linux/arm64`.
*   These binaries will be attached to the corresponding GitHub Release.

## 4. User Experience: The "One-Click" `setup-nexus.sh` Script

A single setup script will fully automate the installation and configuration process.

1.  **Auto-`sudo`:** The script will check for root privileges and, if needed, re-launch itself with `sudo`.
2.  **Prerequisite Check:** The script will check if `systemd` is available and exit gracefully if it is not.
3.  **User Input:** It will prompt the user for their public hostname.
4.  **Automation Steps:**
    *   Detect the machine's architecture.
    *   Download the correct pre-compiled binary from the latest GitHub Release.
    *   Create a `config.yaml` file with the user's hostname and a new, randomly generated `backendsJWTSecret`.
    *   Create a `systemd` service file (`nexus-proxy.service`).
    *   Create a dedicated, unprivileged user to run the service.
    *   Move all files to `/opt/nexus-proxy/`.
    *   Install, enable, and start the `systemd` service.
5.  **Completion:** The script will end by printing a clear, final message:
    ```text
    --------------------------------------------------
    ✅ Nexus Proxy has been successfully installed and started!

    Configuration file saved to: /opt/nexus-proxy/config.yaml

    To connect your backend client, use the following secret:

    backendsJWTSecret: "a-newly-generated-random-secret-string"

    You can check the server status with:
    sudo systemctl status nexus-proxy.service
    --------------------------------------------------
    ```
