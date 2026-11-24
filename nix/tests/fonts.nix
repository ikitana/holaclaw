# The browser image's fonts (nix/fonts.nix): a CJK, an emoji and an Arial
# query each resolve to a font of the set, not to DejaVu.
{ pkgs }:
pkgs.runCommand "holaclaw-fonts"
{
  nativeBuildInputs = [ pkgs.fontconfig ];
  FONTCONFIG_FILE = (import ../fonts.nix { inherit pkgs; }).conf;
} ''
  export XDG_CACHE_HOME=$TMPDIR
  match() {
    got=$(fc-match "$1" family)
    case "$got" in
      "$2"*) ;;
      *) echo "fc-match $1: got $got, want $2" >&2; exit 1 ;;
    esac
  }
  match Arial "Liberation Sans"
  match "Times New Roman" "Liberation Serif"
  match "Courier New" "Liberation Mono"
  match :lang=ja "Noto Sans CJK"
  match :lang=zh-cn "Noto Sans CJK"
  match :lang=ko "Noto Sans CJK"
  match emoji "Noto Color Emoji"
  match sans-serif "DejaVu Sans"
  touch $out
''
