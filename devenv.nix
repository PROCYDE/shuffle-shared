{ pkgs
, lib
, config
, inputs
, ...
}:

{
  # https://devenv.sh/packages/
  packages = with pkgs; [
    gh
    git
  ];

  # https://devenv.sh/languages/
  languages.go.enable = true;
  languages.go.package = pkgs.go;
  languages.go.lsp.enable = true;
  languages.go.lsp.package = pkgs.gopls;

  # https://devenv.sh/integrations/treefmt/
  treefmt = {
    enable = true;
    config = {
      programs = {
        gofmt.enable = true;
        nixpkgs-fmt.enable = true;
      };
    };
  };

  # https://devenv.sh/git-hooks/
  git-hooks.hooks = {
    # Format files via treefmt
    treefmt.enable = true;
  };

  # See full reference at https://devenv.sh/reference/options/
  starship.enable = true;
  starship.package = pkgs.starship;
  starship.config.enable = true;
  starship.config.path = ./starship.toml;

  dotenv.disableHint = true;
}
