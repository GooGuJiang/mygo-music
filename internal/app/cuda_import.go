package app

import (
	"log"

	"github.com/egoist/mygo"

	"mygo-music/internal/player"
)

var chooseCUDAZip = func(parent *mygo.Window) ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: parent, Title: T("Import CUDA Package"), ButtonLabel: T("Import CUDA Package"),
		Filters: []mygo.FileFilter{{Name: T("Zip Archive"), Extensions: []string{"zip"}}},
	})
}

var importCUDAZip = player.ImportDemucsCUDA

func (a *app) startCUDAImport() {
	if a.cudaImporting {
		return
	}
	a.cudaImporting, a.cudaImportMessage = true, ""
	parent := a.settingsWin
	go func() {
		paths, err := chooseCUDAZip(parent)
		selected := err == nil && len(paths) > 0
		if selected {
			err = importCUDAZip(paths[0])
		}
		a.post(func() {
			a.cudaImporting = false
			switch {
			case err != nil:
				log.Printf("importing CUDA package: %v", err)
				a.cudaImportMessage = T("Could not import the CUDA package.")
			case selected:
				a.pl.RefreshDemucs()
				a.cudaImportMessage = T("CUDA package imported. Select Auto or CUDA to use it from the next track.")
			}
		})
	}()
}
