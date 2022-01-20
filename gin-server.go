package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	_ "unsafe"

	"github.com/gin-gonic/gin"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
)

/*
	Written by Salvador Gutierrez salvador.gvz@gmail.com Jan 2022

	This is a server that provides endpoints to process PDFs. The core library used
	is `pdfcpu` https://pdfcpu.io/.

	Configuration yaml file for pdfcpu: config: /Users/<username>/Library/Application Support/pdfcpu/config.yml
	created in first run of pdfcpu ^

	Styling guide (my own):
	- Variables are snake case: foo_bar
	- Structs are title case: FooBar
	- Functions names are camel case: fooBar <--- this is bad, they won't get exported


*/

//>> STRUCTS
type Response struct {
	Status  int
	Message []string
	Error   []string
}

type GenerateRequest struct {
	Context    map[string]interface{} `json: "context_json_file"`
	Output     string                 `json: "output_file"`
	InputFiles []interface{}          `json: "input_files"`
}

type Object interface {
	fmt.Stringer
	Clone() Object
	PDFString() string
}

func main() {
	var port = ":6666"

	// Routes
	r := gin.Default()

	r.GET("/healthcheck", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"Health": "Good!"})
	})

	r.POST("/scrape", scrapeHandler)

	r.POST("/generate", generateHandler)

	r.Run(port)
}

//>> HANDLERS
func generateHandler(c *gin.Context) {
	fmt.Println("in generate")

	//Using jsonDecoder is best practice since it reads the streaming json data (which means it can error out immediately)
	var json_data map[string]interface{}

	//Using jsonDecoder is best practice since it reads the streaming json data (which means it can error out immediately)
	decoder := json.NewDecoder(c.Request.Body)
	err := decoder.Decode(&json_data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err})
	} else {
		// Massage data for /generate fn
		// Here we are converting from interface{} into an array of string interface{}
		context, ok := json_data["context_json_file"].(map[string]interface{})
		if !ok {
			panic("inner map is not a map!")
		}
		// Here we are converting from interface{} into []interface{} into []string
		files_interface := json_data["input_files"].([]interface{})
		files_list := make([]string, len(files_interface))
		for i, v := range files_interface {
			files_list[i] = v.(string)
		}
		var out_path = fmt.Sprintf("%v", json_data["output_file"])
		generate(context, out_path, files_list)

		//c.JSON(http.StatusOK, data_struct)
	}

}

func scrapeHandler(c *gin.Context) {
	/*
		This handler should only handle getting data from request
		and sending it to scrape fn
	*/
	fmt.Println("in scrape")

	// parse request body
	// map[keyType]valueType <---they're like dictionaries
	var json_data map[string]interface{}

	//Using jsonDecoder is best practice since it reads the streaming json data (which means it can error out immediately)
	decoder := json.NewDecoder(c.Request.Body)
	err := decoder.Decode(&json_data)
	if err != nil {
		errorHandler(0, err, c)
	}

	acro_fields := scrape(json_data, c)
	if acro_fields != nil {
		c.JSON(http.StatusOK, gin.H{"acro_form_fields": acro_fields})
	} else {
		c.JSON(http.StatusInternalServerError, "There was a problem reading/writing one or more of the specified PDF files.")
	}
}

/*
	Passing by value in Go may be significantly cheaper than passing by pointer.
 	This happens because Go uses escape analysis to determine if variable can be safely allocated on function’s stack frame
*/
func errorHandler(idx int, err error, c *gin.Context) {
	var unmarshalErr *json.UnmarshalTypeError
	if err != nil {
		if errors.As(err, &unmarshalErr) {
			sendResponse(c, Response{Status: http.StatusBadRequest, Error: []string{fmt.Sprintf("Bad Request. Wrong Type provided for field %v for idx: %d", unmarshalErr.Field, idx)}})
		} else {
			sendResponse(c, Response{Status: http.StatusBadRequest, Error: []string{fmt.Sprintf("Bad Request %v for idx: %d", err.Error(), idx)}})
		}
		return
	}
}

/*
	Generic response handler
*/
func sendResponse(c *gin.Context, response Response) {
	if len(response.Message) > 0 {
		c.JSON(response.Status, map[string]interface{}{"message": strings.Join(response.Message, "; ")})
	} else if len(response.Error) > 0 {
		c.JSON(response.Status, map[string]interface{}{"error": strings.Join(response.Error, "; ")})
	}
}

//>> FUNCTIONS

func scrape(file_dict map[string]interface{}, c *gin.Context) []string {
	/*
		TODO: I don't like the error handling here, redoit all so that we don't use the *gin.Context here at all
		(should only be used in the handler)

		Gets AcroForm data from files and returns a list of fields
			["foo_bar","bar_mitzvah"]
	*/

	/*
		parse the json to be a an array of strings (each string a file path)
		the types inside the slice are not string, they're also interface{}.
		One has to iterate the collection then do a type assertion on each item like so:
	*/
	files_interface := file_dict["files"].([]interface{})
	files_list := make([]string, len(files_interface))
	for i, v := range files_interface {
		files_list[i] = v.(string)
	}

	// TODO make this a batch process
	/*
		This command checks inFile for compliance with the specification PDF 32000-1:2008 (PDF 1.7).
		 Any PDF file you would like to process needs to pass validation.
	*/

	// This is how you create an array of variable length
	acro_fields := make([]string, 0)
	for idx, f := range files_list {
		// Print the file and idx
		//fmt.Println(idx, f)

		//this uses an io.ReadSeeker
		f, err := os.Open(f)

		if err != nil {
			errorHandler(idx, err, c)
		} else {
			//Validate, for all pdfcpu api calls requiring configuration, we can use default
			err = api.Validate(f, nil)
			if err != nil {
				errorHandler(idx, err, c)
			} else {
				// Get AcroForm fields
				f.Seek(0, io.SeekStart)
				res := getAcro(idx, f, &acro_fields)
				if res == 0 {
					continue
				}
				//Close the file this ain't python!
				defer f.Close()

			}
		}
	}
	return acro_fields
}

func generate(context map[string]interface{}, out_dir string, input_files []string) {
	/*
		Fills a PDF's forms (acro form) with user information.
	*/
	// fmt.Printf("Context: %v", context)
	// fmt.Printf("Output: %v", out_dir)
	// fmt.Printf("Input: %v", input_files)

}

//>>HELPERS

func getAcro(idx int, source io.ReadSeeker, acro_fields *[]string) int {
	ctx, err := api.ReadContext(source, nil)
	if err != nil {
		log.Println(idx, err)
		return 0
	}

	cat, err := ctx.Catalog()
	if err != nil {
		log.Println(idx, err)
		return 0
	}

	acroform, ok := cat.Find("AcroForm")
	if !ok {
		log.Printf("No forms for %v with idx: %d", source, idx)
		return 0
	}

	adict, err := ctx.DereferenceDict(acroform)
	if err != nil {
		log.Println(idx, err)
		return 0
	}

	fields := adict.ArrayEntry("Fields")

	for i, o := range fields {
		ir := o.(pdfcpu.IndirectRef)
		e, ok := ctx.FindTableEntryForIndRef(&ir)
		if !ok {
			log.Printf("No XrefTableEntry for %v with idx: %d", ir, idx)
			return 0
		}
		//fmt.Printf("E TYPE: %T", e)
		d, ok := e.Object.(pdfcpu.Dict)
		if !ok {
			log.Printf("Object %v is not a Dict with idx: %d", ir, idx)
			return 0
		}
		//fmt.Printf("INSIDE: %v", d)
		v := d.StringEntry("T")
		if v == nil {
			log.Printf("No field name for field %v with idx: %d", i, idx)
			return 0
		}

		field_name := *v
		*acro_fields = append(*acro_fields, field_name)
		// create object
		//var test Object
		d.Update("V", pdfcpu.String("STUFF!"))
		//d.Update("V", )
		//fmt.Printf("NEW VALUE: %v", d)
		//fmt.Printf("TYPE: %T", d.StringEntry("V"))
		//mergeAcroForms(ctx, ctx)
		//api.WriteContextFile(ctx, "TESTINGFILE.pdf")

	}
	ctx.Write.DirName = "."
	ctx.Write.FileName = "tezzting.pdf"
	pdfcpu.Write(ctx)
	return 1
}

//go:linkname contains pdfcpu.mergeAcroForms
func mergeAcroForms(ctxSource, ctxDest *pdfcpu.Context) error {
	rootDictDest, err := ctxDest.Catalog()
	if err != nil {
		return err
	}

	rootDictSource, err := ctxSource.Catalog()
	if err != nil {
		return err
	}

	o, found := rootDictSource.Find("AcroForm")
	if !found {
		return nil
	}

	dSrc, err := ctxSource.DereferenceDict(o)
	if err != nil || len(dSrc) == 0 {
		return err
	}

	// Retrieve ctxSrc AcroForm Fields
	o, found = dSrc.Find("Fields")
	if !found {
		return nil
	}
	arrFieldsSrc, err := ctxDest.DereferenceArray(o)
	if err != nil {
		return err
	}
	if len(arrFieldsSrc) == 0 {
		return nil
	}

	// We have a ctxSrc.Acroform with fields.

	o, found = rootDictDest.Find("AcroForm")
	if !found {
		rootDictDest["AcroForm"] = dSrc
		return nil
	}

	dDest, err := ctxDest.DereferenceDict(o)
	if err != nil {
		return err
	}

	if len(dDest) == 0 {
		rootDictDest["AcroForm"] = dSrc
		return nil
	}

	// Retrieve ctxDest AcroForm Fields
	o, found = dDest.Find("Fields")
	if !found {
		rootDictDest["AcroForm"] = dSrc
		return nil
	}
	arrFieldsDest, err := ctxDest.DereferenceArray(o)
	if err != nil {
		return err
	}
	if len(arrFieldsDest) == 0 {
		rootDictDest["AcroForm"] = dSrc
		return nil
	}

	// Merge Dsrc into dDest.

	// Fields: add all indrefs

	// Merge all fields from ctxSrc into ctxDest
	arrFieldsDest = append(arrFieldsDest, arrFieldsSrc...)
	dDest["Fields"] = arrFieldsDest

	return handleFormAttributes(ctxSource, ctxDest, dSrc, dDest, arrFieldsSrc)
}

func handleNeedAppearances(ctxSource *pdfcpu.Context, dSrc, dDest pdfcpu.Dict) error {
	o, found := dSrc.Find("NeedAppearances")
	if !found || o == nil {
		return nil
	}
	b, err := ctxSource.DereferenceBoolean(o, pdfcpu.V10)
	if err != nil {
		return err
	}
	if b != nil && *b {
		dDest["NeedAppearances"] = pdfcpu.Boolean(true)
	}
	return nil
}

func handleSigFields(ctxSource, ctxDest *pdfcpu.Context, dSrc, dDest pdfcpu.Dict) error {
	o, found := dSrc.Find("SigFields")
	if !found {
		return nil
	}
	iSrc, err := ctxSource.DereferenceInteger(o)
	if err != nil {
		return err
	}
	if iSrc == nil {
		return nil
	}
	// Merge SigFields into dDest.
	o, found = dDest.Find("SigFlags")
	if !found {
		dDest["SigFields"] = pdfcpu.Integer(*iSrc)
		return nil
	}
	iDest, err := ctxDest.DereferenceInteger(o)
	if err != nil {
		return err
	}
	if iDest == nil {
		dDest["SigFields"] = pdfcpu.Integer(*iSrc)
		return nil
	}
	// SignaturesExist
	if *iSrc&1 > 0 {
		*iDest |= 1
	}
	// AppendOnly
	if *iSrc&2 > 0 {
		*iDest |= 2
	}
	return nil
}

func handleCO(ctxSource, ctxDest *pdfcpu.Context, dSrc, dDest pdfcpu.Dict) error {
	o, found := dSrc.Find("CO")
	if !found {
		return nil
	}
	arrSrc, err := ctxSource.DereferenceArray(o)
	if err != nil {
		return err
	}
	o, found = dDest.Find("CO")
	if !found {
		dDest["CO"] = arrSrc
		return nil
	}
	arrDest, err := ctxDest.DereferenceArray(o)
	if err != nil {
		return err
	}
	if len(arrDest) == 0 {
		dDest["CO"] = arrSrc
	} else {
		arrDest = append(arrDest, arrSrc...)
		dDest["CO"] = arrDest
	}
	return nil
}

func handleDR(ctxSource, ctxDest *pdfcpu.Context, dSrc, dDest pdfcpu.Dict) error {
	o, found := dSrc.Find("DR")
	if !found {
		return nil
	}
	dSrc, err := ctxSource.DereferenceDict(o)
	if err != nil {
		return err
	}
	if len(dSrc) == 0 {
		return nil
	}
	o, found = dDest.Find("DR")
	if !found {
		dDest["DR"] = dSrc
	} else {
		dDest, err := ctxDest.DereferenceDict(o)
		if err != nil {
			return err
		}
		if len(dDest) == 0 {
			dDest["DR"] = dSrc
		}
	}
	return nil
}

func handleDA(ctxSource *pdfcpu.Context, dSrc, dDest pdfcpu.Dict, arrFieldsSrc pdfcpu.Array) error {
	// (for each with field type  /FT /Tx w/o DA, set DA to default DA)
	// TODO Walk field tree and inspect terminal fields.

	sSrc := dSrc.StringEntry("DA")
	if sSrc == nil || len(*sSrc) == 0 {
		return nil
	}
	sDest := dDest.StringEntry("DA")
	if sDest == nil {
		dDest["DA"] = pdfcpu.StringLiteral(*sSrc)
		return nil
	}
	// Push sSrc down to all top level fields of dSource
	for _, o := range arrFieldsSrc {
		d, err := ctxSource.DereferenceDict(o)
		if err != nil {
			return err
		}
		n := d.NameEntry("FT")
		if n != nil && *n == "Tx" {
			_, found := d.Find("DA")
			if !found {
				d["DA"] = pdfcpu.StringLiteral(*sSrc)
			}
		}
	}
	return nil
}

func handleQ(ctxSource *pdfcpu.Context, dSrc, dDest pdfcpu.Dict, arrFieldsSrc pdfcpu.Array) error {
	// (for each with field type /FT /Tx w/o Q, set Q to default Q)
	// TODO Walk field tree and inspect terminal fields.

	iSrc := dSrc.IntEntry("Q")
	if iSrc == nil {
		return nil
	}
	iDest := dDest.IntEntry("Q")
	if iDest == nil {
		dDest["Q"] = pdfcpu.Integer(*iSrc)
		return nil
	}
	// Push iSrc down to all top level fields of dSource
	for _, o := range arrFieldsSrc {
		d, err := ctxSource.DereferenceDict(o)
		if err != nil {
			return err
		}
		n := d.NameEntry("FT")
		if n != nil && *n == "Tx" {
			_, found := d.Find("Q")
			if !found {
				d["Q"] = pdfcpu.Integer(*iSrc)
			}
		}
	}
	return nil
}

func handleFormAttributes(ctxSource, ctxDest *pdfcpu.Context, dSrc, dDest pdfcpu.Dict, arrFieldsSrc pdfcpu.Array) error {

	// NeedAppearances: try: set to true only
	if err := handleNeedAppearances(ctxSource, dSrc, dDest); err != nil {
		return err
	}

	// SigFlags: set bit 1 to true only (SignaturesExist)
	//           set bit 2 to true only (AppendOnly)
	if err := handleSigFields(ctxSource, ctxDest, dSrc, dDest); err != nil {
		return err
	}

	// CO: add all indrefs
	if err := handleCO(ctxSource, ctxDest, dSrc, dDest); err != nil {
		return err
	}

	// DR: default resource dict
	if err := handleDR(ctxSource, ctxDest, dSrc, dDest); err != nil {
		return err
	}

	// DA: default appearance streams for variable text fields
	if err := handleDA(ctxSource, dSrc, dDest, arrFieldsSrc); err != nil {
		return err
	}

	// Q: left, center, right for variable text fields
	if err := handleQ(ctxSource, dSrc, dDest, arrFieldsSrc); err != nil {
		return err
	}

	// XFA: ignore
	delete(dDest, "XFA")

	return nil
}
