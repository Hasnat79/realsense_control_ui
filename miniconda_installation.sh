#!/usr/bin/env bash
curl -fsSL https://repo.anaconda.com/miniconda/Miniconda3-latest-Linux-x86_64.sh >/dev/null 2>&1
set -euo pipefail

# 1. Update packages (optional but recommended)
sudo apt update -y && sudo apt upgrade -y

# 2. Go to home and define install variables
cd "$HOME"
MINICONDA_DIR="$HOME/miniconda3"
MINICONDA_SCRIPT="$HOME/miniconda.sh"
MINICONDA_URL="https://repo.anaconda.com/miniconda/Miniconda3-latest-Linux-x86_64.sh"

# 3. Download latest Miniconda installer
wget "$MINICONDA_URL" -O "$MINICONDA_SCRIPT"

# 4. Run installer in batch mode (no prompts), install into $MINICONDA_DIR
bash "$MINICONDA_SCRIPT" -b -u -p "$MINICONDA_DIR"

# 5. Remove installer script
rm "$MINICONDA_SCRIPT"

# 6. Initialize conda for bash (change 'bash' to 'zsh' if you use zsh)
eval "$("$MINICONDA_DIR/bin/conda" shell.bash hook)"
"$MINICONDA_DIR/bin/conda" init bash

# 7. Activate base env in this shell
eval "$("$MINICONDA_DIR/bin/conda" shell.bash hook)"
conda activate base

# 8. Print conda info as a quick sanity check
conda --version
conda info

echo
echo "Miniconda installed to: $MINICONDA_DIR"
echo "Restart your terminal or run:"
echo "  eval \"\$(\"$MINICONDA_DIR/bin/conda\" shell.bash hook)\""
echo "to use conda in new shells."
