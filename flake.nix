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

        # How to get the vendorHash:
        # Option 1: set to pkgs.lib.fakeHash and paste the value at build time
        # Option 2: go mod vendor && nix hash path --sri ./vendor
        vendorHash = "sha256-1/J3IRNa0hP6PQVE6++rHSTt6cz36eGVFpUSNG6pIIo=";

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
