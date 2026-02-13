---
title: Manual Approvals
---

While automatic reconciliation is generally the ideal, it is not always the 
practical solution. Integrations to infrastructure providers can be shaky or 
slow and if it takes 30 minutes to recover from a bad `apply` or `destroy` 
then you might want human intervention before these actions occur.

A `Workspace` resource has two settings that allow you to control how 
aggressive the automation of the reconciliation is. The `autoApply: <true;
false>` and the `destroy: <auto;manual;skip>` settings.

## Human intervention before running apply
Setting `autoApply: true` means that whenever a difference between desired 
and actual state is detected, a `terraform apply` is automatically run. 

If you set `autoApply: false`, you will have to manually trigger the apply by 
adding the annotation `tf-reconcile.lego.com/manual-apply: true` to the 
`Workspace` resource. After a `terraform apply` action has successfully run, 
the annotation will be removed from the `Workspace` resource to make sure it 
only triggers a single apply.

## Human intervention before running destroy 
Setting `destroy: auto` means that whenever a `Workspace` resource is 
deleted, the `tf-reconcile.lego.com/finalizer` finalizer will block deletion 
until a `terraform destroy` has successfully run.

You can also get a behaviour that is similar to `autoApply: false` when you 
set `destroy: manual` and then you have to add the annotation 
`tf-reconcile.lego.com/manual-destroy: true` to the `Workspace` resource in 
order to trigger the `terraform destroy` action.

If you want to avoid `terraform destroy` actions to run entirely you can set 
`destroy: skip`. In this case, the `tf-reconcile.lego.com/finalizer` 
finalizer has no effect.