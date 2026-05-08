{
  description = "Kubernetes operator for managing infrastructure as code using Terraform";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = [
      "x86_64-linux"
      "aarch64-linux"
      "aarch64-darwin"
    ];
    forAllSystems = nixpkgs.lib.genAttrs systems;
  in {
    packages = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
      krec = pkgs.buildGoModule {
        pname = "krec";
        version = "nix";
        src = ./.;
        subPackages = ["."];

        # This hash must be refreshed whenever go.mod or go.sum change.
        # Refresh workflow:
        # 1. Run `nix build .#krec`. The build fails with a hash
        #    mismatch listing a `got: sha256-...` line.
        # 2. Replace `pkgs.lib.fakeHash` below with that value as a
        #    quoted SRI string (e.g. "sha256-abc...=").
        # `nix-prefetch` and `nix-update` can automate the refresh.
        vendorHash = "sha256-7oXuw88kHgTvs2MEu5tmNMUK1DbbK0YyYK62EBjAaIw=";

        env.CGO_ENABLED = "0";

        ldflags = [
          "-s"
          "-w"
          # NOTE: Using tag would make the nix build impure. Using git commit and date
          "-X github.com/LEGO/kube-tf-reconciler/cmd.version=nix"
          "-X github.com/LEGO/kube-tf-reconciler/cmd.commit=${self.shortRev or "dirty"}"
          "-X github.com/LEGO/kube-tf-reconciler/cmd.date=${self.lastModifiedDate or "19700101000000"}"
        ];

        postInstall = ''
          mv $out/bin/kube-tf-reconciler $out/bin/krec
        '';

        meta = {
          description = "Kubernetes operator for managing infrastructure as code using Terraform";
          homepage = "https://github.com/LEGO/kube-tf-reconciler";
          license = pkgs.lib.licenses.asl20;
          mainProgram = "krec";
        };
      };
    in {
      inherit krec;
      default = krec;
    });

    devShells = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = pkgs.mkShell {
        packages = with pkgs; [
          go
          kubectl
          kind
        ];
      };
    });
  };
}
