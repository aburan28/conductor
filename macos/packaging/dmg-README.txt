Conductor @VERSION@ for macOS
=============================

Double-click "Install Conductor.pkg". It installs Conductor.app in Applications and links
the conductor, conductord and conductor-mcp commands into /usr/local/bin.

Then open Conductor. It starts its control plane and a private PostgreSQL database on this
Mac (nothing listens beyond this Mac), signs you in, and walks you through picking a
repository and connecting your coding tools. Both keep running when you close the window,
and start again when you log in.

To remove it: quit Conductor, then

  launchctl bootout gui/$(id -u)/dev.conductor.daemon
  launchctl bootout gui/$(id -u)/dev.conductor.postgres
  launchctl bootout gui/$(id -u)/dev.conductor.db-backup
  rm ~/Library/LaunchAgents/dev.conductor.*.plist
  sudo rm -rf /Applications/Conductor.app /usr/local/bin/conductor /usr/local/bin/conductord /usr/local/bin/conductor-mcp

Your data stays in ~/Library/Application Support/Conductor and ~/.conductor until you delete
them.
