# Install the node connector

The connector is installed and updated with the `incus-payincus` deb from [Rain-kl/incus](https://github.com/Rain-kl/incus). Supported targets are Debian 13 and Ubuntu 24.04 on amd64 / arm64.

Generate a node installation command in the control panel. Run it on a fresh node, then confirm the Panel address and one-time token in `incus-tui`, initialize storage, configure the HTTPS listener and import the Panel public certificate. The token is masked and redeemed only after confirmation. Without an interactive terminal, run `sudo incus-tui` later and enter the connection details manually.

`latest` selects stable packages; `canary` selects verified integration packages. Panel installation defaults to canary. Basic initialization uses DIR; ZFS setup requires explicit confirmation.

There is no standalone Agent binary, self-updater, legacy protocol fallback or node migration. Update the Incus package through incus-tui / apt; existing legacy nodes require fresh installation.
