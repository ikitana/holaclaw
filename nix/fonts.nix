# The fonts every deployment gives the browser: DejaVu,
# Liberation for pages that ask for Arial, Times New Roman or Courier New,
# Noto CJK and Noto Color Emoji. `conf` is a fontconfig file with only these,
# for the image, which has no /etc/fonts: it includes fontconfig's own conf.d,
# whose aliases map Arial to Liberation Sans and `sans-serif` to DejaVu Sans.
{ pkgs, target ? pkgs }:
rec {
  packages = with target; [
    dejavu_fonts
    liberation_ttf
    noto-fonts-cjk-sans
    noto-fonts-color-emoji
  ];
  conf = pkgs.makeFontsConf {
    fontDirectories = packages;
    impureFontDirectories = [ ];
    includes = [ "${target.fontconfig.out}/etc/fonts/conf.d" ];
  };
}
